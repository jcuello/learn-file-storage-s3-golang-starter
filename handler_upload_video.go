package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	uploadLimit := 1 << 30 // 1GB
	http.MaxBytesReader(w, r.Body, int64(uploadLimit))

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	videoInfo, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to fetch video", err)
		return
	}

	if videoInfo.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", err)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not get access to video file", err)
		return
	}
	defer file.Close()

	mediaType := header.Header.Get("Content-Type")
	// This was returning [.f4v .lrv .m4v .mp4] for some reason, just hard-coding .mp4
	// extensions, err := mime.ExtensionsByType(mediaType)
	// if err != nil || len(extensions) == 0 {
	// 	respondWithError(w, http.StatusInternalServerError, "Could not determine file type.", err)
	// 	return
	// }

	parsedMediaType, _, err := mime.ParseMediaType(mediaType)
	if parsedMediaType != "video/mp4" {
		respondWithError(w, http.StatusInternalServerError, "Unsupported file type.", err)
		return
	}

	tmpFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to create temp file.", err)
		return
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	_, err = io.Copy(tmpFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to copy file.", err)
		return
	}

	processedFilepath, err := processVideoForFastStart(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to process file.", err)
		return
	}

	processedFile, err := os.Open(processedFilepath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to open processed file.", err)
		return
	}
	defer os.Remove(processedFile.Name())
	defer processedFile.Close()

	randomBytes := make([]byte, 32)
	_, err = rand.Read(randomBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not create video name.", err)
		return
	}

	videoName := base64.RawURLEncoding.EncodeToString(randomBytes)
	videoFilename := fmt.Sprintf("%v.mp4", videoName)

	aspectRatio, err := getVideoAspectRatio(processedFile.Name())
	videoKeyPrefix := "other"
	if aspectRatio == "16:9" {
		videoKeyPrefix = "landscape"
	}
	if aspectRatio == "9:16" {
		videoKeyPrefix = "portrait"
	}

	keyName := fmt.Sprintf("%v/%v", videoKeyPrefix, videoFilename)
	_, err = cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &keyName,
		Body:        processedFile,
		ContentType: &parsedMediaType,
	})
	if err != nil {
		respondWithError(w, http.StatusBadGateway, "Failed to upload video", err)
		return
	}

	videoUrl := fmt.Sprintf("https://%v.s3.%v.amazonaws.com/%v", cfg.s3Bucket, cfg.s3Region, keyName)
	videoInfo.VideoURL = &videoUrl
	err = cfg.db.UpdateVideo(videoInfo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoInfo)
}

const (
	aspect16x9 = 1.7777
	aspect9x16 = 0.5625
)

func getVideoAspectRatio(filePath string) (string, error) {
	ffprobeCmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	bytesBuffer := bytes.Buffer{}
	ffprobeCmd.Stdout = &bytesBuffer

	err := ffprobeCmd.Run()
	if err != nil {
		return "", err
	}

	ffprobe := ffprobeResult{}
	err = json.Unmarshal(bytesBuffer.Bytes(), &ffprobe)

	if err != nil {
		return "", err
	}

	if len(ffprobe.Streams) == 0 {
		return "", errors.New("no stream data found")
	}

	aspectRatio := ffprobe.Streams[0].Width / ffprobe.Streams[0].Height
	epsilon := 0.001

	if areAlmostEqual(aspectRatio, aspect16x9, epsilon) {
		return "16:9", nil
	} else if areAlmostEqual(aspectRatio, aspect9x16, epsilon) {
		return "9:16", nil
	} else {
		return "other", nil
	}
}

func areAlmostEqual(a, b, epsilon float64) bool {
	if a == b {
		return true
	}
	return math.Abs(a-b) < epsilon
}

type ffprobeResult struct {
	Streams []struct {
		Width  float64 `json:"width"`
		Height float64 `json:"height"`
	} `json:"streams"`
}

func processVideoForFastStart(filePath string) (string, error) {
	tempPath := filePath + ".processing"
	ffpmegCmd := exec.Command(
		"ffmpeg", "-i", filePath, "-c", "copy", "-movflags",
		"faststart", "-f", "mp4", tempPath)

	err := ffpmegCmd.Run()
	if err != nil {
		return "", err
	}

	return tempPath, nil
}
