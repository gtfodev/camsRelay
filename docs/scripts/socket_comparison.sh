#!/bin/bash
# Socket-level comparison between ffmpeg and our client
# Monitors TCP socket options and states

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

if [ -z "$1" ]; then
    echo -e "${RED}ERROR: RTSP URL required${NC}"
    echo "Usage: $0 '<rtsp_url>'"
    exit 1
fi

RTSP_URL="$1"
CAMERA_HOST="stream-ue1-delta.dropcam.com"

echo -e "${GREEN}=== Socket Comparison Test ===${NC}"
echo ""

# Function to get socket info for a process
get_socket_info() {
    local pid=$1
    local name=$2

    echo -e "${YELLOW}Socket info for ${name} (PID: ${pid}):${NC}"

    # Get sockets
    ss -tn | grep "$CAMERA_HOST" | head -5

    # Get socket options via /proc (if process still exists)
    if [ -d "/proc/$pid" ]; then
        echo "TCP info:"
        ss -tni | grep -A 10 "$CAMERA_HOST" | head -20
    fi
    echo ""
}

# Test 1: ffmpeg socket monitoring
echo -e "${GREEN}=== Test 1: ffmpeg Socket Monitoring ===${NC}"

# Start ffmpeg in background
timeout 5 ffmpeg -rtsp_transport tcp -i "${RTSP_URL}" -f null - &
FFMPEG_PID=$!
sleep 2

get_socket_info $FFMPEG_PID "ffmpeg"

wait $FFMPEG_PID 2>/dev/null || true
echo ""

# Test 2: Our client socket monitoring
echo -e "${GREEN}=== Test 2: Go Client Socket Monitoring ===${NC}"

# Build if needed
if [ ! -f "bin/dev-relay" ]; then
    echo "Building dev-relay..."
    go build -o bin/dev-relay cmd/dev-relay/main.go
fi

# Start dev-relay in background
timeout 5 ./bin/dev-relay &
RELAY_PID=$!
sleep 2

get_socket_info $RELAY_PID "relay"

wait $RELAY_PID 2>/dev/null || true
echo ""

# Test 3: Compare strace system calls
echo -e "${GREEN}=== Test 3: System Call Comparison ===${NC}"
echo "This will show socket options being set..."
echo ""

echo -e "${YELLOW}ffmpeg setsockopt calls:${NC}"
timeout 3 strace -e setsockopt ffmpeg -rtsp_transport tcp -i "${RTSP_URL}" -f null - 2>&1 | \
    grep setsockopt | head -20 || echo "No setsockopt calls found"
echo ""

echo -e "${YELLOW}Go client setsockopt calls:${NC}"
timeout 3 strace -e setsockopt ./bin/dev-relay 2>&1 | \
    grep setsockopt | head -20 || echo "No setsockopt calls found"
echo ""

echo -e "${GREEN}=== Comparison Complete ===${NC}"
echo ""
echo "Look for differences in:"
echo "1. TCP socket states (ESTABLISHED, etc.)"
echo "2. setsockopt calls (TCP_NODELAY, SO_KEEPALIVE, etc.)"
echo "3. Send/Recv queue sizes"
echo "4. Window sizes"
