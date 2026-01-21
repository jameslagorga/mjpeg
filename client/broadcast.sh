#!/bin/bash

# This script starts multiple camera clients to stream to the MJPEG service.
# It dynamically finds available cameras, filters out unwanted ones, and maps
# logical IDs (0, 1, 2) to the correct physical camera indices.

# Ensure the script runs from its own directory.
cd "$(dirname "$0")"

# --- Configuration ---
# The URL of your MJPEG service
MJPEG_URL="${MJPEG_URL:-https://binocular.lagorgeo.us/mjpeg/stream}"

# Logical camera IDs to stream (space-separated). These are 0-indexed and
# will be mapped to the available physical cameras after filtering.
LOGICAL_IDS="0 1 2 3"

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

# --- Find and Filter Cameras ---
echo "--- Discovering and filtering cameras ---"
# Get the list of physical camera indices from ffmpeg, excluding the built-in camera.
# We read the lines, find the ones that are video devices, remove the MacBook camera,
# and extract the numerical index.
AVAILABLE_INDICES=($(ffmpeg -f avfoundation -list_devices true -i "" 2>&1 | \
    grep "AVFoundation video devices" -A 10 | \
    grep -E '\[[0-9]+\]' | \
    grep -v "MacBook Air Camera" | \
    sed 's/.*\[\([0-9]*\)\].*/\1/'))

if [ ${#AVAILABLE_INDICES[@]} -eq 0 ]; then
    echo "Error: No suitable cameras found after filtering."
    exit 1
fi

echo "Found ${#AVAILABLE_INDICES[@]} suitable cameras at physical indices: ${AVAILABLE_INDICES[*]}"
echo "-------------------------------------------"
echo

# Build the client
echo "Building client..."
go build -o client .
if [ $? -ne 0 ]; then
    echo "Error: Failed to build client"
    exit 1
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
for logical_id in $LOGICAL_IDS; do
  if [ "$logical_id" -ge "${#AVAILABLE_INDICES[@]}" ]; then
      echo "Warning: Logical camera ID $logical_id is out of bounds (only ${#AVAILABLE_INDICES[@]} cameras available). Skipping."
      continue
  fi

  # Map the logical ID (0, 1, 2...) to the actual physical camera index found.
  physical_id=${AVAILABLE_INDICES[$logical_id]}

  echo "Starting stream for logical camera ID '${logical_id}' (physical ID: ${physical_id}) to ${MJPEG_URL}"

  LOG_FILE="${LOG_DIR}/client_camera_${logical_id}.log"
  echo "Logging to ${LOG_FILE}"

  # Build the command
  CMD="./client -camera-id ${physical_id} -url ${MJPEG_URL}"
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
for logical_id in $LOGICAL_IDS; do
  echo "  tail -f ${LOG_DIR}/client_camera_${logical_id}.log"
done






