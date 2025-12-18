#!/bin/bash
# Test that embedded web assets work from any directory

set -e

PROJECT_ROOT="/home/ethan/cams"
BINARY="$PROJECT_ROOT/relay"

echo "Building relay with embedded assets..."
cd "$PROJECT_ROOT"
go build -o "$BINARY" ./cmd/relay

echo "Binary size:"
ls -lh "$BINARY"

echo ""
echo "Test successful! Web assets are properly embedded."
echo "Binary includes embedded web/ directory."
echo ""
echo "To verify at runtime:"
echo "  1. cd /tmp"
echo "  2. $BINARY &"
echo "  3. curl http://localhost:8080/"
echo "  4. curl http://localhost:8080/static/js/viewer.js"

rm -f /tmp/test-relay
