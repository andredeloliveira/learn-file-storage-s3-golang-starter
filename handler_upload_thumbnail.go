package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
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

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(
			w,
			http.StatusInternalServerError,
			fmt.Sprintf("Could not fetch video with videoId: %d", videoID),
			err,
		)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized", nil)
		return
	}

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	// TODO: implement the upload here
	const maxMemory = 10 << 20

	r.ParseMultipartForm(maxMemory)
	file, header, err := r.FormFile("thumbnail")

	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse thumbnail file", err)
		return
	}
	defer file.Close()

	mimeTypeRaw := header.Header.Get("Content-Type")
	mimeType, _, err := mime.ParseMediaType(mimeTypeRaw)
	if err != nil {
		respondWithError(
			w,
			http.StatusInternalServerError,
			"Could not parse mime type",
			err,
		)
		return
	}
	if !strings.Contains(mimeType, "image") {
		respondWithError(
			w,
			http.StatusUnsupportedMediaType,
			"Unsupported media type",
			err,
		)
		return
	}

	fmt.Println("what is the file type")

	fileType := strings.Split(mimeType, "/")
	if len(fileType) > 2 {
		respondWithError(
			w,
			http.StatusInternalServerError,
			"unexpected mimeType format",
			fmt.Errorf("mime format error"),
		)
		return
	}

	thumbnailType := fileType[1]
	randomVideoID := []byte(videoIDString)
	_, err = rand.Read(randomVideoID)
	if err != nil {
		respondWithError(
			w,
			http.StatusInternalServerError,
			"could not create random filename from videoID",
			err,
		)
		return
	}
	encodedVideoID := base64.RawURLEncoding.EncodeToString(randomVideoID)
	fmt.Println("encoded videoID sucessfully")
	fmt.Println(encodedVideoID)

	filePathUrl := fmt.Sprintf(
		"/%s.%s",
		encodedVideoID,
		thumbnailType,
	)

	filePath := filepath.Join(cfg.assetsRoot, filePathUrl)

	newFile, err := os.Create(filePath)

	if err != nil {
		respondWithError(
			w,
			http.StatusInternalServerError,
			"could not create file to store thumbnail",
			err,
		)
		return
	}

	_, err = io.Copy(newFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not copy file for thumbnail url", err)
		return
	}

	thumbnailURL := fmt.Sprintf(
		"http://localhost:%s/assets/%s.%s",
		cfg.port,
		encodedVideoID,
		thumbnailType,
	)

	video.ThumbnailURL = &thumbnailURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not update thumbnail url", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}
