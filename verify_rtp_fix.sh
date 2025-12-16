#!/bin/bash

# Verify RTP Packet Fix
# This script checks that the producer waits for WebRTC connection before sending packets

echo "=== RTP Packet Fix Verification ==="
echo ""
echo "Starting test server..."
./test-server > server.log 2>&1 &
SERVER_PID=$!

echo "Server PID: $SERVER_PID"
echo "Waiting 10 seconds for initialization..."
sleep 10

echo ""
echo "=== Checking for correct behavior in logs ==="
echo ""

# Check 1: Wait for connection log appears
if grep -q "waiting for WebRTC connection to be established" server.log; then
    echo "✓ Producer waits for WebRTC connection"
else
    echo "✗ Producer does NOT wait for WebRTC connection"
fi

# Check 2: Connection established before RTSP
if grep -q "WebRTC connection established, starting RTSP stream" server.log; then
    echo "✓ RTSP starts after WebRTC connection"
else
    echo "✗ RTSP may start before WebRTC connection"
fi

# Check 3: First frame written successfully
if grep -q "first video frame written successfully" server.log; then
    echo "✓ First frame written successfully"
else
    echo "✗ No successful frame write detected"
fi

# Check 4: No write errors with wrong state
if grep -q "failed to write.*connection_state=connecting" server.log; then
    echo "✗ ERROR: Attempting to write packets while still connecting"
else
    echo "✓ No attempts to write while connecting"
fi

# Check 5: Connection state in logs
echo ""
echo "=== Connection State Transitions ==="
grep "connection_state=" server.log | head -20

echo ""
echo "=== Stopping test server ==="
kill $SERVER_PID
wait $SERVER_PID 2>/dev/null

echo ""
echo "Full logs saved to: server.log"
echo "Open http://localhost:8080 to test viewer"
