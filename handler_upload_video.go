package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"

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

	randomBytes := make([]byte, 32)
	_, err = rand.Read(randomBytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not create video name.", err)
		return
	}

	videoName := base64.RawURLEncoding.EncodeToString(randomBytes)
	videoFilename := fmt.Sprintf("%v.mp4", videoName)
	tmpFile.Seek(0, io.SeekStart)
	_, err = cfg.s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &videoFilename,
		Body:        tmpFile,
		ContentType: &parsedMediaType,
	})
	if err != nil {
		respondWithError(w, http.StatusBadGateway, "Failed to upload video", err)
		return
	}

	videoUrl := fmt.Sprintf("https://%v.s3.%v.amazonaws.com/%v", cfg.s3Bucket, cfg.s3Region, videoFilename)
	videoInfo.VideoURL = &videoUrl
	err = cfg.db.UpdateVideo(videoInfo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoInfo)
}
