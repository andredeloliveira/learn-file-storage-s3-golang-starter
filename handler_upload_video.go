package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Could not find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Could not validate JWT", err)
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

	fmt.Println("uploading media for video", videoID, "by user", userID)

	const maxMemory = 10 << 30 // 1GB

	r.ParseMultipartForm(maxMemory)

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse media file", err)
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

	if !strings.Contains(mimeType, "video") {
		respondWithError(
			w,
			http.StatusUnsupportedMediaType,
			"Unsupported media type",
			err,
		)
		return
	}

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

	mediaType := fileType[1]
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

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(
			w,
			http.StatusInternalServerError,
			"could not create file to store media",
			err,
		)
		return
	}

	defer os.Remove(tempFile.Name())
	defer tempFile.Close()
	_, err = io.Copy(tempFile, file)

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not copy file for media url", err)
		return
	}

	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(
			w,
			http.StatusInternalServerError,
			"could not set pointer to start of file",
			err,
		)
		return
	}

	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(
			w,
			http.StatusInternalServerError,
			"Could not get Video Aspect Ratio",
			err,
		)
		return
	}

	fastStartFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(
			w,
			http.StatusInternalServerError,
			"Could not process 'Fast Start Video' file",
			err,
		)
		return
	}

	fastStartFile, err := os.Open(fastStartFilePath)
	if err != nil {
		respondWithError(
			w,
			http.StatusInternalServerError,
			"Could not process 'Fast Start Video' file",
			err,
		)
		return
	}
	defer fastStartFile.Close()
	// I don't think I need to seek for the start of the file, but lets keep it for now
	fastStartFile.Seek(0, io.SeekStart)

	s3VideoKey := fmt.Sprintf("%s/%s.%s", aspectRatio, encodedVideoID, mediaType)

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &s3VideoKey,
		ContentType: &mimeType,
		Body:        fastStartFile,
	})

	if err != nil {
		respondWithError(
			w,
			http.StatusInternalServerError,
			"could not upload video media to s3",
			err,
		)
		return
	}

	mediaURL := strings.Join([]string{
		cfg.s3Bucket, s3VideoKey,
	}, ",")

	video.VideoURL = &mediaURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not update video url", err)
		return
	}

	videoDTO, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not get video presigned url", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoDTO)
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}
	splittedURL := strings.Split(*video.VideoURL, ",")
	if len(splittedURL) != 2 {
		return database.Video{}, errors.New("Could not get bucket and Key from video URL")
	}

	presignedURL, err := generatePresignedURL(
		cfg.s3Client,
		splittedURL[0],
		splittedURL[1],
		time.Minute*1,
	)
	if err != nil {
		return database.Video{}, err
	}
	newVideo := video
	newVideo.VideoURL = &presignedURL
	return newVideo, nil
}

// gets a file, executes ffprobe to get the data, and returns
// a string representing "16:9", "9:16" or "other" as apect ratios
func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command(
		"ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		filePath,
	)

	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	type videoSizes struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}

	type fFProbeStreams struct {
		Streams []videoSizes `json:"streams"`
	}

	var data fFProbeStreams

	err = json.Unmarshal(output, &data)
	if err != nil {
		return "", fmt.Errorf("Could not unmarshal ffprobe response %v", err)
	}

	width := float64(data.Streams[0].Width)
	height := float64(data.Streams[0].Height)
	aspectRatio := width / height
	fmt.Println("aspect ratio", aspectRatio)
	if aspectRatio >= 1.7 && aspectRatio <= 1.78 {
		return "portrait", nil
	}
	if aspectRatio >= 0.5 && aspectRatio <= 0.58 {
		return "landscape", nil
	}
	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	newProcessingFilepath := filePath + ".processing"
	fmt.Println(newProcessingFilepath)

	cmd := exec.Command(
		"ffmpeg",
		"-i", filePath,
		"-c", "copy",
		"-movflags", "faststart",
		"-f", "mp4",
		newProcessingFilepath,
	)

	_, err := cmd.Output()
	if err != nil {
		return "", err
	}

	return newProcessingFilepath, nil
}

func generatePresignedURL(
	s3Client *s3.Client,
	bucket,
	key string,
	expireTime time.Duration,
) (string, error) {
	options := s3.WithPresignExpires(expireTime)
	presignClient := s3.NewPresignClient(s3Client, options)
	presignObj, err := presignClient.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		return "", err
	}

	return presignObj.URL, nil
}
