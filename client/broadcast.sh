#!/bin/bash

# This script starts multiple camera clients to stream to the MJPEG service.
# It can start clients for multiple cameras in parallel.

# Ensure the script runs from its own directory.
cd "$(dirname "$0")"

# --- Configuration ---
# The URL of your MJPEG service
MJPEG_URL="${MJPEG_URL:-http://binocular.lagorgeo.us:80/mjpeg/stream}"

# Camera unique IDs to stream (space-separated)
CAMERA_IDS="0 1 2"



# Verbose logging (set to "true" to enable)

VERBOSE="${VERBOSE:-false}"



# --- Pre-flight Checks ---

if ! command -v go &> /dev/null; then

    echo "Error: Go is not installed. Please install it to run this script."

    exit 1

fi



if ! command -v ffmpeg &> /dev/null; then

    echo "Error: ffmpeg is not installed. Please install it to run this script."

    echo "On macOS, you can install it with: brew install ffmpeg"

    exit 1

fi



# Build the client if it doesn't exist

if [ ! -f "./client" ]; then

    echo "Building client..."

    go build -o client .

    if [ $? -ne 0 ]; then

        echo "Error: Failed to build client"

        exit 1

    fi

fi



# --- Pre-flight Cleanup ---

echo "--- Ensuring a clean start ---"

echo "Terminating any existing client processes..."

pkill -f "./client" > /dev/null 2>&1 || true

sleep 1

echo



# --- Process Management Setup ---

# File to store the PIDs of the background client processes for easy cleanup

PID_FILE="pids-client.txt"

> "$PID_FILE" # Clear the PID file at the start



# Create the directory for logs if it doesn't exist

LOG_DIR="logs"

mkdir -p "$LOG_DIR"



# --- Main Loop: Start Streaming ---

for id in $CAMERA_IDS; do

  echo "Starting stream for camera with ID '${id}' to ${MJPEG_URL}"



  LOG_FILE="${LOG_DIR}/client_camera_${id}.log"

  echo "Logging to ${LOG_FILE}"



  # Build the command

  CMD="./client -camera-id ${id} -url ${MJPEG_URL}"

  if [ "$VERBOSE" = "true" ]; then

    CMD="${CMD} -verbose"

  fi



  # Run the client in the background using nohup to ensure it keeps running

  # when the shell is closed. Redirect its stdout and stderr to a log file.

  nohup $CMD > "$LOG_FILE" 2>&1 &

  

    CLIENT_PID=$!

  

    echo "$CLIENT_PID" >> "$PID_FILE"

  

    echo "  -> Started background process with PID: $CLIENT_PID"

  

    echo "Waiting 1 second before starting next camera..."

  

    sleep 1

  

  done



echo

echo "All camera streams started."

echo "PIDs of background processes are stored in ${PID_FILE}."

echo "To stop all streams, run: kill \$(cat \"${PID_FILE}\")"

echo

echo "To view logs:"

for id in $CAMERA_IDS; do

  echo "  tail -f ${LOG_DIR}/client_camera_${id}.log"

done






