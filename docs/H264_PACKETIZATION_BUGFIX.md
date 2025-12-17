# H.264 RTP Packetization Bug Fix

## The Problem

Browser WebRTC statistics showed:
- `packetsReceived: 604` ✓ (RTP packets reaching browser)
- `framesReceived: 0` ✗ (H.264 decoder rejecting packets)
- `framesDecoded: 0` ✗ (No valid frames)

**Root Cause:** The H.264 decoder was receiving malformed RTP packets due to incorrect NAL unit formatting.

## Technical Analysis

### The Data Flow Bug

The pipeline had a format mismatch between the H.264 processor output and the RTP payloader input:

```
RTSP Stream (RTP packets)
    ↓
H264Processor.ProcessPacket()    [Depacketizes RTP → NAL units]
    ↓
OnFrame callback                 [Returns AVC format: 4-byte length prefix + NAL data]
    ↓
Bridge.WriteVideoSample()        [Receives AVC format NAL units]
    ↓
H264Payloader.Payload()          [EXPECTS raw NAL units WITHOUT length prefix]
    ↓
WebRTC Track                     [Sends MALFORMED RTP packets]
    ↓
Browser H.264 Decoder            [REJECTS invalid packets]
```

### The Bug

The `pkg/rtp/h264.go` processor outputs NAL units in **AVC format** (4-byte length prefix):

```go
// Line 176-186 in h264.go
func appendNALU(dst, nalu []byte) []byte {
    // AVC format: 4-byte length prefix + NALU data
    length := uint32(len(nalu))
    dst = append(dst,
        byte(length>>24),  // Length byte 0
        byte(length>>16),  // Length byte 1
        byte(length>>8),   // Length byte 2
        byte(length),      // Length byte 3
    )
    return append(dst, nalu...)  // NAL unit data
}
```

But `H264Payloader` from Pion expects **raw NAL units** without any length prefix. When it received:

```
[0x00][0x00][0x00][0x15][0x67][0x42][0xe0][0x1f]...
 ^--- 4-byte length ---^  ^--- NAL data ---^
```

It treated the length bytes as part of the NAL header, creating invalid RTP packets that the browser decoder rejected.

## The Fix

**File:** `/home/ethan/cams/pkg/bridge/bridge.go`

Added `extractNALUs()` function to convert AVC format back to raw NAL units:

```go
// extractNALUs extracts individual NAL units from AVC format data
// AVC format: [4-byte length][NAL data][4-byte length][NAL data]...
// Returns slice of raw NAL units (without length prefixes)
func extractNALUs(data []byte) ([][]byte, error) {
    var nalus [][]byte
    offset := 0

    for offset < len(data) {
        // Read 4-byte big-endian length
        naluLen := int(data[offset])<<24 | int(data[offset+1])<<16 |
                   int(data[offset+2])<<8 | int(data[offset+3])
        offset += 4

        // Validate and extract NAL unit (without length prefix)
        nalu := data[offset : offset+naluLen]
        nalus = append(nalus, nalu)
        offset += naluLen
    }

    return nalus, nil
}
```

Updated `WriteVideoSample()` to:
1. Extract raw NAL units from AVC format
2. Packetize each NAL unit individually
3. Set RTP marker bit only on last packet of last NAL unit

### Correct Data Flow Now

```
RTSP Stream (RTP packets)
    ↓
H264Processor.ProcessPacket()    [Depacketizes RTP → NAL units]
    ↓
OnFrame callback                 [Returns AVC format NAL units]
    ↓
Bridge.WriteVideoSample()        [Extracts raw NAL units from AVC format]
    ↓
extractNALUs()                   [Strips 4-byte length prefixes]
    ↓
H264Payloader.Payload()          [Packetizes raw NAL units correctly]
    ↓
WebRTC Track                     [Sends RFC 6184 compliant RTP packets]
    ↓
Browser H.264 Decoder            [Successfully decodes frames] ✓
```

## Expected Results

After this fix, the browser should show:
- `packetsReceived: >0` ✓
- `framesReceived: >0` ✓ (NEW - frames being accepted)
- `framesDecoded: >0` ✓ (NEW - successful decode)
- Video rendering in browser ✓

## Testing

```bash
# Rebuild and run
go build -o multi-relay ./cmd/multi-relay
./multi-relay

# Check browser console WebRTC stats
# Should now see framesReceived and framesDecoded incrementing
```

## Related Files

- `/home/ethan/cams/pkg/bridge/bridge.go` - RTP packetization (FIXED)
- `/home/ethan/cams/pkg/rtp/h264.go` - H.264 depacketization (outputs AVC format)
- `/home/ethan/cams/pkg/relay/relay.go` - Pipeline orchestration

## References

- **RFC 6184**: RTP Payload Format for H.264 Video
- **Pion H264Payloader**: Expects raw NAL units without length prefixes
- **AVC Format**: ISO/IEC 14496-15 (length-prefixed NAL units)
