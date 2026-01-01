package main

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"flag"

	"github.com/gin-gonic/gin"
)

// archiveFrame holds the data for a single frame to be written to the TAR archive.
type archiveFrame struct {
	Name string
	Data []byte
}

var verbose *bool

func main() {
	log.Println("Starting mjpeg-service")

	verbose = flag.Bool("verbose", false, "Enable verbose ffmpeg logs.")
	flag.Parse()

	router := gin.Default()

	router.Use(func(c *gin.Context) {
		log.Printf("Incoming request: Method=%s, Path=%s", c.Request.Method, c.Request.URL.Path)
		c.Next()
	})

	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.POST("/stream/:stream_name", handleStream)
	router.GET("/image/:stream_name/:timestamp", handleImageRequest)

	router.Run(":8080")
}

func handleImageRequest(c *gin.Context) {
	streamName := c.Param("stream_name")
	timestampStr := c.Param("timestamp")

	timestampMs, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid timestamp format"})
		return
	}

	jpegPath := filepath.Join("/mnt/nfs/streams/jpeg", streamName)

	files, err := os.ReadDir(jpegPath)
	if err != nil {
		log.Printf("Failed to read directory %s: %v", jpegPath, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not read stream directory"})
		return
	}

	var bestTarPath string
	var maxTarTimestampMs int64 = -1

	for _, file := range files {
		if file.IsDir() {
			continue
		}
		fileName := file.Name()
		if strings.HasSuffix(fileName, ".tar") {
			base := strings.TrimSuffix(fileName, ".tar")
			parts := strings.Split(base, "_")
			if len(parts) < 2 {
				continue
			}
			tarTimestampStr := parts[len(parts)-1]
			tarTimestampMs, err := strconv.ParseInt(tarTimestampStr, 10, 64)
			if err != nil {
				continue
			}

			// Find the latest tar file whose timestamp is <= the requested image timestamp
			if tarTimestampMs <= timestampMs && tarTimestampMs > maxTarTimestampMs {
				maxTarTimestampMs = tarTimestampMs
				bestTarPath = filepath.Join(jpegPath, fileName)
			}
		}
	}

	if bestTarPath == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "No archive file found covering the given timestamp"})
		return
	}

	file, err := os.Open(bestTarPath)
	if err != nil {
		log.Printf("Failed to open tar file %s: %v", bestTarPath, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open archive file"})
		return
	}
	defer file.Close()

	r := tar.NewReader(file)

	var lastValidFrameData []byte

	for {
		hdr, err := r.Next()
		if err == io.EOF {
			break // End of archive
		}
		if err != nil {
			log.Printf("Error reading tar header in %s: %v", bestTarPath, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read archive file"})
			return
		}

		if hdr.Typeflag == tar.TypeReg {
			frameNameWithoutExt := strings.TrimSuffix(hdr.Name, ".jpg")
			frameTimestampMs, err := strconv.ParseInt(frameNameWithoutExt, 10, 64)
			if err != nil {
				log.Printf("Warning: Could not parse timestamp from frame name %s in tar: %v", hdr.Name, err)
				// Discard data and continue to next entry if timestamp is unparseable
				if _, err := io.Copy(io.Discard, r); err != nil {
					log.Printf("Error discarding invalid frame data for %s: %v", hdr.Name, err)
				}
				continue
			}

			if frameTimestampMs <= timestampMs {
				// This frame is a candidate. Read its data.
				buf := new(bytes.Buffer)
				if _, err := io.Copy(buf, r); err != nil {
					log.Printf("Error reading frame data for %s: %v", hdr.Name, err)
					continue // Skip this frame if data can't be read
				}
				lastValidFrameData = buf.Bytes()
			} else {
				// This frame's timestamp is *greater than* the requested timestamp.
				// Since frames are chronological, any subsequent frames will also be too new.
				// So, we have found the latest valid frame in `lastValidFrameData` (if any).
				// We can break here.
				if _, err := io.Copy(io.Discard, r); err != nil { // Still need to discard this one
					log.Printf("Error discarding frame data for %s: %v", hdr.Name, err)
				}
				break
			}
		} else {
			// Discard non-regular file entries
			if _, err := io.Copy(io.Discard, r); err != nil {
				log.Printf("Error discarding non-regular entry %s: %v", hdr.Name, err)
			}
		}
	}

	if lastValidFrameData != nil {
		c.Header("Content-Type", "image/jpeg")
		c.Status(http.StatusOK)
		if _, err := c.Writer.Write(lastValidFrameData); err != nil {
			log.Printf("Error writing image data to response: %v", err)
		}
		return
	}

	c.JSON(http.StatusNotFound, gin.H{"error": "No image found in archive matching or preceding the timestamp"})
}

func handleStream(c *gin.Context) {
	streamName := c.Param("stream_name")
	if streamName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "stream_name is required"})
		return
	}

	jpegPath := filepath.Join("/mnt/nfs/streams/jpeg", streamName)
	hlsPath := filepath.Join("/mnt/nfs/streams/hls", streamName)

	// Create directories for the streams and clean up old ones
	if err := os.RemoveAll(jpegPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not clean up JPEG stream directory"})
		return
	}
	if err := os.MkdirAll(jpegPath, 0755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not create JPEG stream directory"})
		return
	}
	if err := os.RemoveAll(hlsPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not clean up HLS stream directory"})
		return
	}
	if err := os.MkdirAll(hlsPath, 0755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not create HLS stream directory"})
		return
	}

	// Channel to send frames to the archiver. Buffered to handle temporary slowdowns.
	archiveCh := make(chan archiveFrame, 300) // Buffer for ~60 seconds of frames at 5fps

	// Start the background worker to write frames to a TAR archive.
	go archiveWriter(c.Request.Context(), jpegPath, streamName, archiveCh)

	// --- FFMPEG setup ---
	ffmpegArgs := []string{
		"-f", "mjpeg",
		"-framerate", "5",
		"-i", "-",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-tune", "zerolatency",
		"-crf", "23",
		"-g", "10",
		"-hls_time", "2",
		"-hls_list_size", "5",
		"-hls_flags", "delete_segments",
		"-flush_packets", "1",
		"-hls_segment_filename", filepath.Join(hlsPath, "segment%03d.ts"),
		filepath.Join(hlsPath, "playlist.m3u8"),
	}
	if !*verbose {
		ffmpegArgs = append([]string{"-loglevel", "error"}, ffmpegArgs...)
	}
	cmd := exec.CommandContext(c.Request.Context(), "ffmpeg", ffmpegArgs...)

	ffmpegStdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("Failed to get stdin pipe for ffmpeg: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to setup ffmpeg"})
		return
	}
	defer ffmpegStdin.Close()

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start ffmpeg: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start ffmpeg"})
		return
	}

	ffmpegDone := make(chan error, 1)
	go func() {
		ffmpegDone <- cmd.Wait()
	}()

	// --- Multipart processing ---
	mediaType, params, err := mime.ParseMediaType(c.Request.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid Content-Type, must be multipart/*"})
		return
	}

	mr := multipart.NewReader(c.Request.Body, params["boundary"])

	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if c.Request.Context().Err() == nil {
				log.Printf("Error reading multipart part: %v", err)
			}
			break
		}

		clientTimestamp := p.Header.Get("X-Client-Timestamp")
		if clientTimestamp == "" {
			log.Printf("Multipart part missing X-Client-Timestamp header.")
			continue
		}

		// Read the frame into a buffer so we can send it to multiple places.
		var frameData bytes.Buffer
		if _, err := io.Copy(&frameData, p); err != nil {
			log.Printf("Error reading frame data: %v", err)
			continue
		}

		// Non-blockingly send the frame to the archiver.
		select {
		case archiveCh <- archiveFrame{Name: clientTimestamp + ".jpg", Data: frameData.Bytes()}:
			// Frame sent for archival
		default:
			log.Printf("Archive channel is full. Dropping frame %s for archival to prioritize live stream.", clientTimestamp)
		}

		// Write the frame to ffmpeg for HLS processing.
		if _, err := ffmpegStdin.Write(frameData.Bytes()); err != nil {
			if c.Request.Context().Err() == nil {
				log.Printf("Error writing frame to ffmpeg: %v", err)
			}
			break
		}
	}

	// Signal the archiver that no more frames are coming.
	close(archiveCh)

	// Ffmpeg will exit once its stdin is closed.
	err = <-ffmpegDone
	if err != nil && c.Request.Context().Err() != context.Canceled {
		log.Printf("ffmpeg command finished with error: %v", err)
	}

	log.Printf("Finished processing stream for %s", streamName)
	c.Status(http.StatusOK)
}

// archiveWriter receives frames from a channel and writes them to a TAR file,
// creating a new file every 60 seconds.
func archiveWriter(ctx context.Context, path, streamName string, ch <-chan archiveFrame) {
	log.Printf("Starting archive writer for stream %s", streamName)

	var tarFile *os.File
	var tarWriter *tar.Writer
	var currentTarStartMs int64 = -1 // Millisecond timestamp of the first frame in the current tar file.

	// Close the writer and file when the function exits.
	defer func() {
		if tarWriter != nil {
			tarWriter.Close()
		}
		if tarFile != nil {
			tarFile.Close()
		}
		log.Printf("Archive writer for stream %s stopped.", streamName)
	}()

	// newTarFile will now accept the start timestamp (in milliseconds) for the new archive
	newTarFile := func(startMs int64) {
		// Close previous file if it exists.
		if tarWriter != nil {
			tarWriter.Close()
		}
		if tarFile != nil {
			tarFile.Close()
		}

		// Create a new file name with the start timestamp of the first frame in this archive.
		newFileName := filepath.Join(path, streamName+"_"+strconv.FormatInt(startMs, 10)+".tar")

		var err error
		tarFile, err = os.Create(newFileName)
		if err != nil {
			log.Printf("ARCHIVER: Failed to create new tar file %s: %v", newFileName, err)
			tarFile = nil
			tarWriter = nil
			return
		}
		tarWriter = tar.NewWriter(tarFile)
		currentTarStartMs = startMs // Update the start timestamp for the new archive
		log.Printf("ARCHIVER: Created new archive file: %s", newFileName)
	}

	for {
		select {
		case frame, ok := <-ch:
			if !ok {
				// Channel closed, we're done.
				return
			}

			frameTimestampMs, err := strconv.ParseInt(strings.TrimSuffix(frame.Name, ".jpg"), 10, 64)
			if err != nil {
				log.Printf("ARCHIVER: Could not parse timestamp from frame name %s: %v", frame.Name, err)
				continue
			}

			// Check if we need to rotate the archive file.
			// A new archive is started if this is the very first frame, or if more than
			// 60 seconds (60000 milliseconds) have passed since the start of the current archive.
			if currentTarStartMs == -1 || (frameTimestampMs-currentTarStartMs) >= 60000 {
				newTarFile(frameTimestampMs) // Name the new archive with the current frame's timestamp
				if tarWriter == nil {
					log.Printf("ARCHIVER: Halting due to file creation failure.")
					return
				}
			}

			hdr := &tar.Header{
				Name:    frame.Name,
				Size:    int64(len(frame.Data)),
				Mode:    0644,
				ModTime: time.Now(), // This ModTime is for the entry *within* the tar, not the tar file itself.
			}
			if err := tarWriter.WriteHeader(hdr); err != nil {
				log.Printf("ARCHIVER: Failed to write tar header for %s: %v", frame.Name, err)
				continue
			}
			if _, err := tarWriter.Write(frame.Data); err != nil {
				log.Printf("ARCHIVER: Failed to write frame data for %s: %v", frame.Name, err)
				continue
			}
		case <-ctx.Done():
			// The request was cancelled.
			return
		}
	}
}
