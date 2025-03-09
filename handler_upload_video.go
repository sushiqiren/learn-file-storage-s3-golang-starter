package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

// FFProbeResponse represents the JSON response from ffprobe
type FFProbeResponse struct {
	Streams []struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"streams"`
}

func getVideoAspectRatio(filePath string) (string, error) {
	// Create a new buffer to capture stdout
	var stdout bytes.Buffer

	// Create the ffprobe command
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	cmd.Stdout = &stdout

	// Run the command
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("failed to execute ffprobe: %v", err)
	}

	// Parse the JSON output
	var response FFProbeResponse
	err = json.Unmarshal(stdout.Bytes(), &response)
	if err != nil {
		return "", fmt.Errorf("failed to unmarshal ffprobe output: %v", err)
	}

	// Make sure we have stream data
	if len(response.Streams) == 0 {
		return "", fmt.Errorf("no stream information found")
	}

	// Get width and height from the first stream
	width := response.Streams[0].Width
	height := response.Streams[0].Height

	// Check for valid dimensions
	if width <= 0 || height <= 0 {
		return "", fmt.Errorf("invalid dimensions: width=%d, height=%d", width, height)
	}

	// Calculate aspect ratio
	ratio := float64(width) / float64(height)

	// Determine the aspect ratio string
	// 16:9 is approximately 1.78
	// 9:16 is approximately 0.56
	if math.Abs(ratio-1.78) < 0.1 {
		return "16:9", nil
	} else if math.Abs(ratio-0.56) < 0.1 {
		return "9:16", nil
	} else {
		return "other", nil
	}
}

func processVideoForFastStart(filePath string) (string, error) {
	// Create output file path by appending .processing to the input file
	outputFilePath := filePath + ".processing"

	// Create the ffmpeg command with verbose logging
	cmd := exec.Command(
		"ffmpeg",
		"-i", filePath, // Input file
		"-c", "copy", // Copy the streams without re-encoding
		"-movflags", "+faststart", // Add fast start flags for web playback (note the + sign)
		"-f", "mp4", // Ensure output format is mp4
		"-y",           // Overwrite output file if it exists
		outputFilePath, // Output file
	)

	// Capture stderr for debugging
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	// Execute the command
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("failed to process video for fast start: %v, stderr: %s", err, stderr.String())
	}

	// Verify the file was created and is not empty
	fileInfo, err := os.Stat(outputFilePath)
	if err != nil {
		return "", fmt.Errorf("failed to stat processed file: %v", err)
	}

	if fileInfo.Size() == 0 {
		return "", fmt.Errorf("processed file is empty")
	}

	return outputFilePath, nil
}

func verifyFastStart(filePath string) error {
	// Run ffprobe to check moov atom position
	cmd := exec.Command(
		"ffprobe",
		"-v", "error",
		"-show_entries", "format=format_name",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "json",
		filePath,
	)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("failed to verify fast start: %v", err)
	}

	// You could check the output here but it's not a reliable way to verify moov location

	return nil
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const uploadLimit = 1 << 30
	r.Body = http.MaxBytesReader(w, r.Body, uploadLimit)

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
		respondWithError(w, http.StatusInternalServerError, "Couldn't find video", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Not authorized to update this video", nil)
		return
	}

	file, handler, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(handler.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type, only MP4 is allowed", nil)
		return
	}

	// Create a temporary file to save the uploaded video
	tempFile, err := os.CreateTemp("", "tubely-upload-*.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not create temp file", err)
		return
	}
	defer os.Remove(tempFile.Name()) // Clean up original temp file
	defer tempFile.Close()

	// Copy the uploaded file to the temporary file
	if _, err := io.Copy(tempFile, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not write file to disk", err)
		return
	}

	// Get the aspect ratio of the video
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not determine video aspect ratio", err)
		return
	}

	// Determine the prefix based on the aspect ratio
	var prefix string
	switch aspectRatio {
	case "16:9":
		prefix = "landscape"
	case "9:16":
		prefix = "portrait"
	default:
		prefix = "other"
	}

	// Process the video for fast start
	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not process video for fast start", err)
		return
	}
	defer os.Remove(processedFilePath) // Clean up processed temp file

	// Open the processed file for uploading
	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not open processed file", err)
		return
	}
	defer processedFile.Close()

	// Log file info for debugging
	if fileInfo, err := os.Stat(processedFilePath); err == nil {
		fmt.Printf("Processed file size: %d bytes\n", fileInfo.Size())
	}

	// Generate a unique key with the aspect ratio prefix
	fileExt := filepath.Ext(handler.Filename)
	if fileExt == "" {
		fileExt = ".mp4" // Default to .mp4 if no extension
	}
	fileName := strings.ReplaceAll(videoID.String(), "-", "")
	key := fmt.Sprintf("%s/%s%s", prefix, fileName, fileExt)

	// Upload the processed video file to S3
	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(key),
		Body:        processedFile,
		ContentType: aws.String(mediaType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error uploading file to S3", err)
		return
	}

	// Construct the S3 URL
	url := cfg.getObjectURL(key)
	video.VideoURL = &url
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}
