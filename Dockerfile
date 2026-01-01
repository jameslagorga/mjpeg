# --- Build Stage ---
FROM golang:1.23 as builder

WORKDIR /app

# Copy the Go module files and download dependencies first
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code and build the application
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -installsuffix cgo -o mjpeg .

# --- Final Stage ---
FROM jrottenberg/ffmpeg:5.0-ubuntu

WORKDIR /app

# Copy the compiled Go application from the builder stage
COPY --from=builder /app/mjpeg .

ENTRYPOINT ["./mjpeg"]
CMD []




