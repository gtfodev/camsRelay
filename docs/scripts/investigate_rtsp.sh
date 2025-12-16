#!/bin/bash
# RTSP Investigation Script
# Captures wire protocol for both ffmpeg and our Go client to identify differences

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

CAMERA_HOST="stream-ue1-delta.dropcam.com"
INTERFACE="eth2"
RTSP_URL=""

echo -e "${GREEN}=== RTSP Investigation Script ===${NC}"
echo ""

# Check for URL argument
if [ -z "$1" ]; then
    echo -e "${RED}ERROR: RTSP URL required${NC}"
    echo "Usage: $0 '<rtsp_url>'"
    exit 1
fi

RTSP_URL="$1"
echo -e "${YELLOW}Target URL:${NC} ${RTSP_URL}"
echo -e "${YELLOW}Network Interface:${NC} ${INTERFACE}"
echo -e "${YELLOW}Camera Host:${NC} ${CAMERA_HOST}"
echo ""

# Check we're in correct directory
if [ ! -f "cmd/relay/main.go" ]; then
    echo -e "${RED}ERROR: Must run from /home/ethan/cams directory${NC}"
    exit 1
fi

# Cleanup function
cleanup() {
    echo -e "\n${YELLOW}Cleaning up...${NC}"
    # Kill any running tcpdump processes
    sudo killall tcpdump 2>/dev/null || true
    # Kill ffmpeg if still running
    killall ffmpeg 2>/dev/null || true
}
trap cleanup EXIT

# Phase 1: Capture ffmpeg's RTSP conversation
echo -e "${GREEN}=== Phase 1: Capturing ffmpeg RTSP conversation ===${NC}"
echo "Starting tcpdump on ${INTERFACE} filtering for ${CAMERA_HOST}..."

sudo tcpdump -i ${INTERFACE} -w /tmp/ffmpeg_rtsp.pcap "host ${CAMERA_HOST}" &
TCPDUMP_PID=$!
sleep 2  # Give tcpdump time to start

echo "Running ffmpeg for 5 seconds..."
timeout 5 ffmpeg -rtsp_transport tcp -loglevel debug -i "${RTSP_URL}" -t 5 -f null - 2>&1 | tee /tmp/ffmpeg_debug.log || true

# Stop tcpdump
sudo kill ${TCPDUMP_PID} 2>/dev/null || true
sleep 1

echo -e "${GREEN}ffmpeg capture complete${NC}"
echo ""

# Phase 2: Extract ffmpeg headers
echo -e "${GREEN}=== Phase 2: Analyzing ffmpeg RTSP headers ===${NC}"
grep -E "(DESCRIBE|SETUP|PLAY|OPTIONS|User-Agent|Accept|Transport|Range)" /tmp/ffmpeg_debug.log | head -20 || echo "No headers found in log"
echo ""

# Phase 3: Capture our Go client's conversation
echo -e "${GREEN}=== Phase 3: Capturing Go client RTSP conversation ===${NC}"
echo "Building relay..."
go build -o bin/relay cmd/relay/main.go

echo "Starting tcpdump..."
sudo tcpdump -i ${INTERFACE} -w /tmp/relay_rtsp.pcap "host ${CAMERA_HOST}" &
TCPDUMP_PID=$!
sleep 2

echo "Running relay for 10 seconds..."
timeout 10 ./bin/relay 2>&1 | tee /tmp/relay_debug.log || true

# Stop tcpdump
sudo kill ${TCPDUMP_PID} 2>/dev/null || true
sleep 1

echo -e "${GREEN}Go client capture complete${NC}"
echo ""

# Phase 4: Compare pcap files
echo -e "${GREEN}=== Phase 4: Comparing packet captures ===${NC}"
echo -e "${YELLOW}ffmpeg packets:${NC}"
sudo tcpdump -r /tmp/ffmpeg_rtsp.pcap -n -c 50 2>/dev/null | head -20 || true
echo ""

echo -e "${YELLOW}Go client packets:${NC}"
sudo tcpdump -r /tmp/relay_rtsp.pcap -n -c 50 2>/dev/null | head -20 || true
echo ""

# Phase 5: Extract RTSP protocol text from pcaps
echo -e "${GREEN}=== Phase 5: Extracting RTSP protocol text ===${NC}"
echo -e "${YELLOW}ffmpeg RTSP conversation:${NC}"
sudo tcpdump -r /tmp/ffmpeg_rtsp.pcap -A -n 2>/dev/null | grep -A 5 "RTSP" | head -50 || true
echo ""

echo -e "${YELLOW}Go client RTSP conversation:${NC}"
sudo tcpdump -r /tmp/relay_rtsp.pcap -A -n 2>/dev/null | grep -A 5 "RTSP" | head -50 || true
echo ""

# Phase 6: Summary
echo -e "${GREEN}=== Investigation Complete ===${NC}"
echo ""
echo "Files generated:"
echo "  - /tmp/ffmpeg_rtsp.pcap      (ffmpeg packet capture)"
echo "  - /tmp/relay_rtsp.pcap       (Go client packet capture)"
echo "  - /tmp/ffmpeg_debug.log      (ffmpeg debug output)"
echo "  - /tmp/relay_debug.log       (relay debug output)"
echo ""
echo -e "${YELLOW}Next steps:${NC}"
echo "1. Compare RTSP requests visually"
echo "2. Check for missing headers in Go client"
echo "3. Analyze TCP socket behavior differences"
echo ""
echo "For detailed RTSP text comparison:"
echo "  sudo tcpdump -r /tmp/ffmpeg_rtsp.pcap -A -n | less"
echo "  sudo tcpdump -r /tmp/relay_rtsp.pcap -A -n | less"
