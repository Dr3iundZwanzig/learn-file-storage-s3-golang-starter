package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxMemory = 1 << 30
	r.Body = http.MaxBytesReader(w, r.Body, maxMemory)
	if err := r.ParseForm(); err != nil {
		respondWithError(w, http.StatusBadRequest, "over file limit", err)
	}

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

	videoMetadata, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to get video data", err)
		return
	}
	if videoMetadata.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Current user is not the owner", err)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	contentType := header.Header.Get("Content-Type")
	mediatype, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediatype != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Unknowen file extension", err)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to copy file", err)
		return
	}
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable reset pointer of temp file", err)
		return
	}
	randomString, err := makeRandomBase64String()
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to generate file key", err)
		return
	}

	directory := ""
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting ratio", err)
		return
	}
	switch aspectRatio {
	case "16:9":
		directory = "landscape"
	case "9:16":
		directory = "portrait"
	default:
		directory = "other"
	}
	fileKey := randomString + ".mp4"
	fileKey = path.Join(directory, fileKey)

	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video", err)
		return
	}
	defer os.Remove(processedFilePath)
	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error opening processed file", err)
		return
	}
	defer processedFile.Close()

	params := s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &fileKey,
		Body:        processedFile,
		ContentType: &contentType,
	}
	_, err = cfg.s3Client.PutObject(r.Context(), &params)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to put object with s3", err)
		return
	}
	VideoUrl := cfg.s3Bucket + "," + fileKey
	videoMetadata.VideoURL = &VideoUrl
	err = cfg.db.UpdateVideo(videoMetadata)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable update video in database", err)
		return
	}
	videoMetadata, err = cfg.dbVideoToSignedVideo(videoMetadata)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Error converting video to signedVideo", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoMetadata)
	fmt.Println("done uploading")
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmdCommand := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)

	var buffer bytes.Buffer
	cmdCommand.Stdout = &buffer
	err := cmdCommand.Run()
	if err != nil {
		return "", fmt.Errorf("Error running command for video aspect ratio: %v", err)
	}

	var out struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}
	err = json.Unmarshal(buffer.Bytes(), &out)
	if err != nil {
		return "", fmt.Errorf("Error unmashaling command output: %v", err)
	}

	if len(out.Streams) == 0 {
		return "", fmt.Errorf("error no streams found for video")
	}

	videoWidth := out.Streams[0].Width
	videoHeight := out.Streams[0].Height

	if videoWidth == 16*videoHeight/9 {
		return "16:9", nil
	} else if videoHeight == 16*videoWidth/9 {
		return "9:16", nil
	}
	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	newFilePath := fmt.Sprintf("%s.processing", filePath)
	cmdCommand := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", newFilePath)

	err := cmdCommand.Run()
	if err != nil {
		return "", fmt.Errorf("Error running command for video fast start: %v", err)
	}

	return newFilePath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expiredTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)
	presignedHTTPRequest, err := presignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expiredTime))
	if err != nil {
		return "", fmt.Errorf("error creating HTTP request for presigned object:%v", err)
	}
	return presignedHTTPRequest.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}
	explorationTime := time.Duration(time.Minute * 5)
	splitVideoURL := strings.Split(*video.VideoURL, ",")
	if len(splitVideoURL) < 2 {
		return video, nil
	}
	bucket := splitVideoURL[0]
	key := splitVideoURL[1]
	presignedURL, err := generatePresignedURL(cfg.s3Client, bucket, key, explorationTime)
	if err != nil {
		return video, err
	}
	video.VideoURL = &presignedURL
	return video, nil
}
