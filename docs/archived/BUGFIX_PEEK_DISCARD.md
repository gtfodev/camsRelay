# Bug Fix: RTSP PLAY Success but Zero Packets Received

## Problem Statement

The RTSP client successfully completed the PLAY handshake (status 200), but timed out waiting for RTP packets. ffmpeg could stream from the same URL, indicating a client implementation issue.

**Symptoms:**
```
RTSP PLAY response: status=200, rtp_info=url=trackID=1;seq=1;rtptime=...
no buffered data after PLAY response
read timeout - no data from RTSP server (timeout after 10s)
packets_received: 0
```

## Root Cause Analysis

The bug was in `ReadPackets()` method's handling of interleaved RTP packets:

### Original (Broken) Implementation
```go
// In ReadPackets() loop:
peek, err := c.reader.Peek(4)  // Peek at 4 bytes, don't consume
if peek[0] == '$' {
    c.readInterleavedPacket()  // Call separate method
}

// In readInterleavedPacket():
magic, err := c.reader.ReadByte()  // ERROR: Try to read 5th byte!
channelID, err := c.reader.ReadByte()
// ... etc
```

**The Issue:**
1. `Peek(4)` reads bytes but **does not consume them** from buffer
2. `readInterleavedPacket()` then calls `ReadByte()` to read the '$' magic byte
3. This attempts to read **5 bytes total** (4 from Peek + 1 from ReadByte)
4. But the server sends packets in 4-byte aligned chunks: `[$][channel][size_hi][size_lo][payload...]`
5. `Peek(4)` would timeout waiting for a 5th byte that doesn't exist

### Why ffmpeg Worked

ffmpeg's RTSP implementation properly consumes peeked bytes before reading more data. It follows the pattern used in go2rtc.

## Solution

Match go2rtc's implementation pattern from `pkg/rtsp/conn.go`:

```go
// Peek at header (doesn't consume)
buf4, err := c.reader.Peek(4)

// Extract header fields from peeked data
channel := buf4[1]
size := binary.BigEndian.Uint16(buf4[2:4])

// CRITICAL: Discard the 4 peeked bytes before reading more
if _, err := c.reader.Discard(4); err != nil {
    return err
}

// Now read the payload
payload := make([]byte, size)
io.ReadFull(c.reader, payload)
```

## Packet Format

RTP/RTCP interleaved packets over TCP (RFC 2326):
```
Byte 0:    '$' (0x24) - Magic byte
Byte 1:    Channel ID (0=video RTP, 1=video RTCP, 2=audio RTP, 3=audio RTCP)
Byte 2-3:  Payload size (big-endian uint16)
Byte 4+:   Payload data
```

## Implementation Changes

**File:** `/home/ethan/cams/pkg/rtsp/client.go`

**Changes:**
1. Inlined `readInterleavedPacket()` into `ReadPackets()` loop
2. After `Peek(4)`, immediately call `Discard(4)` to consume header bytes
3. Extract channel and size from peeked buffer, not by reading again
4. Added detailed packet format comments

**Removed:**
- `readInterleavedPacket()` method (no longer needed)

## Testing

**Build:**
```bash
go build -o relay ./cmd/relay/
```

**Run:**
```bash
./relay  # Uses .env for configuration
```

**Expected Result:**
- PLAY response received (status 200)
- First RTP packet received within 1-2 seconds
- Continuous packet stream with counts logged every 1000 packets

## References

- go2rtc implementation: `/home/ethan/cams/go2rtc/pkg/rtsp/conn.go` (lines 188-287)
- RFC 2326 (RTSP): Section 10.12 "Embedded (Interleaved) Binary Data"
- bufio.Reader.Peek documentation: Does not advance reader position
- bufio.Reader.Discard documentation: Advances reader by n bytes

## Lessons Learned

1. **Peek != Read**: `Peek()` is non-consuming, must explicitly `Discard()` or read to advance
2. **Byte alignment matters**: TCP streams require precise byte counting
3. **Match working implementations**: go2rtc's battle-tested pattern was the key reference
4. **Test with real cameras**: Nest cameras behave differently than generic RTSP servers
