package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// ImageMeta represents metadata about an image from the Picsum API.
type ImageMeta struct {
	ID          string `json:"id"`
	Author      string `json:"author"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	URL         string `json:"url"`
	DownloadURL string `json:"download_url"`
}

// Result represents the outcome of processing and downloading an image.
type Result struct {
	ID        string        // Image ID
	Author    string        // Author of the image
	Size      string        // Dimensions in WxH format
	Error     error         // Error encountered during processing (if any)
	TimeSpent time.Duration // Duration taken to process the image
}

// global logger instance
var logger = slog.Default()

// imageProcessor reads jobs from the jobs channel, processes each image
// (validation + download), and sends results to the results channel.
// It uses a timeout context for each job and decrements the WaitGroup when done.
func imageProcessor(
	id int,
	jobs <-chan ImageMeta,
	results chan<- Result,
	wg *sync.WaitGroup,
	timeout time.Duration,
) {
	defer wg.Done()

	for job := range jobs {
		startTime := time.Now()
		logger.Info("Worker processing image",
			"worker_id", id,
			"image_id", job.ID,
			"author", job.Author,
		)

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		result := Result{
			ID:     job.ID,
			Author: job.Author,
			Size:   fmt.Sprintf("%dx%d", job.Width, job.Height),
		}

		// First validate the image URL, then download it
		if err := processImageMeta(ctx, job); err != nil {
			result.Error = err
			result.TimeSpent = time.Since(startTime)
			results <- result
			logger.Warn("Validation failed",
				"image_id", job.ID,
				"error", err,
				"time_spent", result.TimeSpent,
			)
			continue
		}

		/* 		if err := downloadImage(ctx, job); err != nil {
			result.Error = err
			result.TimeSpent = time.Since(startTime)
			results <- result
			logger.Warn("Download failed",
				"image_id", job.ID,
				"error", err,
				"time_spent", result.TimeSpent,
			)
			continue
		} */

		result.TimeSpent = time.Since(startTime)
		results <- result
		logger.Info("Image processed successfully",
			"image_id", job.ID,
			"author", job.Author,
			"size", result.Size,
			"time_spent", result.TimeSpent,
		)
	}
}

// processImageMeta performs an HTTP GET request to the image download URL
// to validate that it returns a 200 OK status.
func processImageMeta(ctx context.Context, meta ImageMeta) error {
	req, err := http.NewRequestWithContext(ctx, "GET", meta.DownloadURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request for image %s: %w", meta.ID, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("image %s download check failed: %w", meta.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("image %s returned status %d", meta.ID, resp.StatusCode)
	}

	return nil
}

// downloadImage fetches the image content from the download URL and saves it
// to the local filesystem under the "images/" directory as <ID>.jpg.
func downloadImage(ctx context.Context, meta ImageMeta) error {
	req, err := http.NewRequestWithContext(ctx, "GET", meta.DownloadURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request for image %s: %w", meta.ID, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("image %s download request failed: %w", meta.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("image %s returned HTTP %d", meta.ID, resp.StatusCode)
	}

	if err := os.MkdirAll("images", 0755); err != nil {
		return fmt.Errorf("failed to create images directory: %w", err)
	}

	filePath := filepath.Join("images", fmt.Sprintf("%s.jpg", meta.ID))
	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create file for image %s: %w", meta.ID, err)
	}
	defer file.Close()

	_, err = io.Copy(file, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to save image %s: %w", meta.ID, err)
	}

	return nil
}

// fetchImageList queries the Picsum Photos API to retrieve a list of image metadata.
// It returns a slice of ImageMeta or an error.
func fetchImageList(limit int) ([]ImageMeta, error) {
	url := fmt.Sprintf("https://picsum.photos/v2/list?page=1&limit=%d", limit)
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch image list: %w", err)
	}
	defer resp.Body.Close()

	var images []ImageMeta
	if err := json.NewDecoder(resp.Body).Decode(&images); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	return images, nil
}

// main is the entry point. It sets up the worker pool to concurrently
// validate and download images from a real API.
func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	numWorkers := runtime.NumCPU() * 2
	const jobTimeout = 4 * time.Second

	logger.Info("Starting image downloader", "workers", numWorkers)

	images, err := fetchImageList(10)
	if err != nil {
		logger.Error("Failed to fetch image list", "error", err)
		return
	}

	jobs := make(chan ImageMeta, len(images))
	results := make(chan Result, len(images))
	var wg sync.WaitGroup

	// Fan-Out
	for w := 1; w <= numWorkers; w++ {
		wg.Add(1)
		go imageProcessor(w, jobs, results, &wg, jobTimeout)
	}

	for _, img := range images {
		jobs <- img
	}
	close(jobs)

	// Fan-In
	go func() {
		wg.Wait()
		close(results)
	}()

	// closing a channel only means "no more values will be sent to it."
	// Reading from a closed channel is still safe.
	for result := range results {
		if result.Error != nil {
			logger.Warn("Image processing failed",
				"image_id", result.ID,
				"author", result.Author,
				"error", result.Error,
				"time_spent", result.TimeSpent,
			)
		} else {
			logger.Info("Image processed",
				"image_id", result.ID,
				"author", result.Author,
				"size", result.Size,
				"time_spent", result.TimeSpent,
			)
		}
	}
}
