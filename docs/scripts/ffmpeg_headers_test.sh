#!/bin/bash
# Quick test to see exactly what headers ffmpeg sends

if [ -z "$1" ]; then
    echo "Usage: $0 '<rtsp_url>'"
    exit 1
fi

RTSP_URL="$1"

echo "=== Running ffmpeg with debug logging ==="
echo ""

# Run ffmpeg with verbose debug and capture all RTSP protocol details
ffmpeg -rtsp_transport tcp -loglevel trace -i "${RTSP_URL}" -t 2 -f null - 2>&1 | \
    grep -E "(Sending:|Received:|User-Agent|Accept|Transport|Range|CSeq|Session|PLAY|SETUP|DESCRIBE|OPTIONS)" | \
    head -100

echo ""
echo "=== Key things to check ==="
echo "1. Does ffmpeg send Range header on PLAY?"
echo "2. What User-Agent does ffmpeg use?"
echo "3. Are there any headers after PLAY?"
echo "4. Does ffmpeg send anything special in Transport?"
