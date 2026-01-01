package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// scanJPEG is a custom split function for bufio.Scanner. It finds the EOI
// (End of Image) marker in a stream of JPEG data to split the stream into
// individual frames.
func scanJPEG(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	// The EOI marker for a JPEG file is 0xFFD9.
	eoiMarker := []byte{0xff, 0xd9}
	if i := bytes.Index(data, eoiMarker); i >= 0 {
		// We have found a full frame. The token is the data up to and
		// including the EOI marker.
		return i + len(eoiMarker), data[0 : i+len(eoiMarker)], nil
	}
	// If we're at the end of the stream but haven't found a full frame,
	// it's an incomplete frame. We can just return it.
	if atEOF {
		return len(data), data, nil
	}
	// If we haven't found a marker and we're not at EOF, we need more data.
	return 0, nil, nil
}

func main() {
	log.Println("--- Go MJPEG Multipart Streamer ---")

	// --- 1. Parse Command-Line Flags ---
	cameraID := flag.String("camera-id", "", "Unique ID of the camera device to use.")
	streamURL := flag.String("url", "http://localhost:8080/stream", "URL of the MJPEG service.")
	verbose := flag.Bool("verbose", false, "Enable verbose ffmpeg logs.")
	flag.Parse()

	if *cameraID == "" {
		log.Fatal("camera-id flag is required")
	}

	streamKey := "camera_" + *cameraID
	log.Printf("Starting stream for key '%s' on camera ID %s to URL %s", streamKey, *cameraID, *streamURL)

	// --- 2. Set up signal handling for graceful shutdown ---
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// --- 3. Start the ffmpeg Subprocess ---
	// -f image2pipe outputs a raw stream of JPEG files, which is what we need.
	ffmpegArgs := []string{
		"-re",
		"-f", "avfoundation",
		"-video_size", "1280x720",
		"-framerate", "10",
		"-pix_fmt", "uyvy422",
		"-use_wallclock_as_timestamps", "1",
		"-i", *cameraID + ":none",
		"-vf", "fps=5",
		"-c:v", "mjpeg",
		"-q:v", "5",
		"-f", "image2pipe",
		"-", // Output to stdout
	}
	if !*verbose {
		ffmpegArgs = append([]string{"-loglevel", "error"}, ffmpegArgs...)
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", ffmpegArgs...)

	ffmpegStdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("Failed to get ffmpeg stdout pipe: %v", err)
	}

	cmd.Stderr = os.Stderr // Redirect ffmpeg logs to our stderr

	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start ffmpeg: %v", err)
	}

	// --- 4. Set up the Streaming HTTP POST Request ---
	// We use an io.Pipe to connect our multipart writer directly to the HTTP
	// request body, avoiding buffering the entire stream in memory.
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()

	mpWriter := multipart.NewWriter(pw)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *streamURL+"/"+streamKey, pr)
	if err != nil {
		log.Fatalf("Failed to create HTTP request: %v", err)
	}
	// Set the Content-Type with the correct multipart boundary.
	req.Header.Set("Content-Type", mpWriter.FormDataContentType())

	// Use a channel to wait for the HTTP request to finish.
	httpDone := make(chan struct{})
	go func() {
		defer close(httpDone)
		log.Println("Starting HTTP POST...")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if ctx.Err() == nil { // Don't log error if we cancelled the context
				log.Printf("HTTP request failed: %v", err)
			}
			return
		}
		defer resp.Body.Close()
		log.Printf("HTTP response received: %s", resp.Status)
		// Drain the response body to allow connection reuse.
		io.Copy(io.Discard, resp.Body)
	}()

	// --- 5. Main Loop: Scan, Timestamp, and Stream Frames ---
	// The scanner reads from ffmpeg's stdout, using our custom split function
	// to identify individual JPEG frames.
	scanner := bufio.NewScanner(ffmpegStdout)
	scanner.Split(scanJPEG)
	// Set a large buffer, as frames can be large.
	scanner.Buffer(make([]byte, 2*1024*1024), 4*1024*1024)

	var frameCount int
	go func() {
		for scanner.Scan() {
			frameBytes := scanner.Bytes()
			if len(frameBytes) == 0 {
				continue
			}

			// Create a new part in the multipart stream
			part, err := mpWriter.CreatePart(textproto.MIMEHeader{
				"Content-Type":       []string{"image/jpeg"},
				"X-Client-Timestamp": []string{fmt.Sprintf("%d", time.Now().UnixMilli())},
			})

			if err != nil {
				log.Printf("Failed to create multipart part: %v", err)
				break
			}

			// Write the JPEG data to the part
			if _, err := part.Write(frameBytes); err != nil {
				log.Printf("Failed to write frame to multipart part: %v", err)
				break
			}
			frameCount++
		}

		if err := scanner.Err(); err != nil {
			if ctx.Err() == nil {
				log.Printf("Error reading from ffmpeg stdout: %v", err)
			}
		}

		// Once the scanner is done, close the writers to signal the end of the stream.
		mpWriter.Close()
		pw.Close()
	}()

	// --- 6. Wait for everything to finish ---
	log.Println("Streaming... Press Ctrl+C to stop.")
	<-ctx.Done() // Wait for shutdown signal
	log.Println("Shutdown signal received.")

	// Wait for ffmpeg to exit
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && !exitErr.Success() {
			// ffmpeg will likely exit with an error when we cancel its context, which is normal.
			log.Printf("ffmpeg exited with non-zero status: %v", err)
		}
	}

	pr.Close() // Ensure the pipe reader is closed to unblock the HTTP client

	log.Println("Waiting for HTTP request to complete...")
	<-httpDone

	log.Printf("Stream finished. Sent %d frames.", frameCount)
}



