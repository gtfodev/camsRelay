# Debugging H.264 RTP Packet Issues

Quick guide for debugging the "framesDecoded: 0" issue using the new logging infrastructure.

## Quick Start

### 1. Full Debug with NAL and RTP Logging

```bash
./bin/diagnose --debug-rtp --debug-nal -o h264-debug.log
```

This will capture:
- **RTP packet details**: sequence numbers, timestamps, payload types, sizes
- **NAL unit types**: SPS, PPS, IDR, P-frames with sizes
- **Raw payload bytes**: First 32-64 bytes of each packet/NAL unit in hex

### 2. JSON Format for Structured Analysis

```bash
./bin/diagnose --debug-all --log-format json -o debug.json
```

JSON output can be easily parsed with `jq`:

```bash
# Count NAL unit types
jq -r 'select(.category == "nal") | .type_name' debug.json | sort | uniq -c

# Find all SPS/PPS
jq 'select(.type_name == "SPS" or .type_name == "PPS")' debug.json

# Track RTP sequence numbers
jq -r 'select(.category == "rtp") | .sequence' debug.json | head -20
```

### 3. Debug Specific Categories Only

```bash
# NAL units only (for H.264 structure analysis)
./bin/diagnose --debug-nal -o nal-only.log

# RTP packets only (for sequence/timing analysis)
./bin/diagnose --debug-rtp -o rtp-only.log

# Track status (WebRTC/RTSP tracks)
./bin/diagnose --debug-track -o track-status.log
```

## What to Look For

### 1. Verify SPS/PPS are Present

```bash
grep "SPS" h264-debug.log
grep "PPS" h264-debug.log
```

**Expected**: At least one SPS and one PPS within first few seconds

**If missing**: Decoder cannot initialize. Check RTSP SDP parsing.

### 2. Verify IDR Keyframes are Coming

```bash
grep "IDR" h264-debug.log | head -5
```

**Expected**: IDR frames every 2-4 seconds (GOP interval)

**If missing**: Decoder cannot start. Check if Nest is actually sending video.

### 3. Check NAL Unit Sequence

A valid H.264 stream should show:

```
SPS → PPS → IDR → P-frame → P-frame → ... → IDR → ...
```

Look at the log to verify this pattern:

```bash
grep "category=nal" h264-debug.log | grep "type_name" | head -20
```

### 4. Examine Raw Payload Bytes

For the first SPS/PPS/IDR, check the raw bytes:

```bash
grep -A 1 "NAL payload.*SPS" h264-debug.log | head -5
```

Expected SPS start: `67 64 00 ...` (NAL type 7)
Expected PPS start: `68 ...` (NAL type 8)
Expected IDR start: `65 ...` (NAL type 5)

### 5. Check RTP Packet Continuity

```bash
# Extract sequence numbers
jq -r 'select(.category == "rtp") | .sequence' debug.json | head -50
```

Look for:
- **Gaps**: Missing packets (sequence jumps)
- **Duplicates**: Same sequence number twice
- **Out of order**: Sequence numbers not monotonically increasing

## Common Issues and Solutions

### Issue 1: No SPS/PPS in Log

**Symptom**: No "SPS received" or "PPS received" messages

**Diagnosis**:
```bash
# Check if ANY NAL units are being received
grep "NAL unit" h264-debug.log | head -10
```

**Causes**:
- RTSP SDP parsing failed to extract parameter sets
- NAL unit extraction logic is incorrect
- Camera is not sending H.264

**Solution**: Check `pkg/rtsp` SDP parsing and NAL unit extraction

### Issue 2: SPS/PPS Present but No IDR

**Symptom**: See SPS/PPS but zero IDR keyframes

**Diagnosis**:
```bash
grep "IDR" h264-debug.log
# If empty, check what NAL types we ARE getting:
grep "type_name" h264-debug.log | cut -d'=' -f2 | sort | uniq -c
```

**Causes**:
- Camera is in "motion detection" mode (only sends on motion)
- Stream is audio-only
- NAL type parsing is incorrect (confusing type 1 with type 5)

**Solution**: Force keyframe request or check camera settings

### Issue 3: RTP Packet Loss

**Symptom**: Gaps in sequence numbers

**Diagnosis**:
```bash
# Check for sequence gaps
jq -r 'select(.category == "rtp") | .sequence' debug.json | \
  awk '{if(NR>1 && $1 != prev+1) print "Gap: " prev " -> " $1} {prev=$1}'
```

**Causes**:
- Network packet loss
- RTSP client buffer overflow
- UDP receive buffer too small

**Solution**: Increase buffer sizes, check network

### Issue 4: Fragmented NAL Units

**Symptom**: Many "FU-A" type NAL units, or "fragmented=true"

**Diagnosis**:
```bash
grep "fragmented=true" h264-debug.log | wc -l
```

**Expected**: IDR keyframes are often fragmented (large size)

**If excessive fragmentation**: May indicate MTU issues

**Check**: Ensure FU-A reassembly is working correctly

### Issue 5: Timestamp Issues

**Symptom**: RTP timestamps not incrementing properly

**Diagnosis**:
```bash
jq -r 'select(.category == "rtp") | .timestamp' debug.json | head -50
```

**Expected**: Monotonically increasing, increment of ~3000 per frame at 30fps (90000 Hz clock)

**If stuck**: Clock not advancing properly
**If jumping**: Timestamp calculation error

## Advanced Debugging

### Hex Dump Analysis

For deep inspection of a specific packet:

```bash
# Find a specific sequence number
jq 'select(.sequence == 12345)' debug.json

# Get its payload bytes
jq -r 'select(.sequence == 12345) | .payload_bytes' debug.json
```

Compare against H.264 specification:
- NAL unit type = `payload_bytes[0] & 0x1F`
- FU-A indicator = type 28 (0x1C)
- FU-A header in `payload_bytes[1]`

### Correlation with Browser Stats

Run diagnostic tool, then check browser:

```javascript
// In browser console
pc.getStats().then(stats => {
  stats.forEach(s => {
    if(s.type === 'inbound-rtp' && s.kind === 'video') {
      console.log('packetsReceived:', s.packetsReceived);
      console.log('framesDecoded:', s.framesDecoded);
      console.log('bytesReceived:', s.bytesReceived);
    }
  });
});
```

**Compare**:
- `packetsReceived` (browser) vs `packets_sent` (diagnostic log)
- If packets arrive but framesDecoded=0: decode failure

### Packet Capture Comparison

For ultimate validation, compare against Wireshark:

```bash
# Capture RTSP traffic
sudo tcpdump -i any -w rtsp-capture.pcap 'port 554'

# Analyze in Wireshark
# Filter: rtp && h264
# Compare sequence numbers, timestamps with log
```

## Output Interpretation

### Good H.264 Stream Log

```
time=... level=INFO msg=">>> SPS received" count=1 size=28 elapsed=245ms
time=... level=INFO msg=">>> PPS received" count=1 size=9 elapsed=246ms
time=... level=INFO msg=">>> IDR KEYFRAME received" count=1 size=15234 interval=0s elapsed=250ms
time=... level=DEBUG msg="RTP packet" category=rtp sequence=1 timestamp=90000 payload_type=96 payload_size=1200
time=... level=DEBUG msg="NAL unit" category=nal type=5 type_name=IDR size=1200 fragmented=true
...
time=... level=INFO msg=">>> IDR KEYFRAME received" count=2 size=14832 interval=2.1s elapsed=2350ms
```

### Problematic Log

```
# No SPS/PPS - critical error
time=... level=INFO msg=">>> P-frames received" count=1000
# (missing SPS/PPS/IDR)

# OR

# SPS/PPS but no IDR - no keyframes
time=... level=INFO msg=">>> SPS received" count=1 size=28
time=... level=INFO msg=">>> PPS received" count=1 size=9
time=... level=INFO msg=">>> P-frames received" count=5000
# (missing IDR)
```

## Performance Notes

- `--debug-rtp` logs ~30 packets/second (minimal overhead)
- `--debug-nal` logs ~5-10 NAL units/second
- `--debug-all` can generate 50-100 MB/hour of logs
- Use specific categories to reduce overhead

## Next Steps After Debugging

1. **If SPS/PPS missing**: Fix RTSP SDP parameter set extraction
2. **If IDR missing**: Check camera configuration or force keyframe
3. **If packets arrive but framesDecoded=0**:
   - Check NAL unit order
   - Verify SPS/PPS sent before IDR
   - Check for malformed NAL units
4. **If everything looks correct**:
   - Browser compatibility issue
   - WebRTC track configuration mismatch
   - Check codec profile/level compatibility

## Reference

- H.264 NAL unit types: See ITU-T H.264 specification Section 7.3.1
- RTP payload format: RFC 6184 (H.264 RTP Payload Format)
- Full logging docs: [LOGGING.md](./LOGGING.md)
