# Test Checklist - RTSP Packet Reception Fix

## Pre-Test Verification

- [x] Code compiled successfully: `relay` binary built
- [x] Critical fix in place: `Discard(4)` after `Peek(4)`
- [x] Old `readInterleavedPacket()` method removed
- [x] Packet format documented with comments

## Test Execution

Run the relay:
```bash
./relay 2>&1 | tee test_fix.log
```

## Expected Behavior Timeline

**T+0s (Startup):**
```json
{"level":"INFO","msg":"starting Nest Camera → Cloudflare SFU relay"}
{"level":"INFO","msg":"connecting to RTSP server","scheme":"rtsps"}
```

**T+1-2s (RTSP Handshake):**
```json
{"level":"INFO","msg":"OPTIONS response"}
{"level":"INFO","msg":"received SDP"}
{"level":"INFO","msg":"track setup complete","channel":0,"type":"video"}
{"level":"INFO","msg":"track setup complete","channel":2,"type":"audio"}
```

**T+2-3s (PLAY):**
```json
{"level":"INFO","msg":"sent RTSP PLAY request"}
{"level":"INFO","msg":"starting packet read loop"}
{"level":"INFO","msg":"RTSP PLAY response received","status":200}
```

**T+3-5s (CRITICAL - First Packet):**
```json
{"level":"INFO","msg":"received first RTP packet successfully"}
```
⚠️ **PASS/FAIL**: If this appears within 5 seconds → FIX SUCCESSFUL
⚠️ **FAIL**: If "read timeout" appears instead → Issue persists

**T+5-60s (Streaming):**
```json
{"level":"INFO","msg":"packets received","count":1000}
{"level":"INFO","msg":"packets received","count":2000}
...
```

## Success Criteria

1. ✅ No "read timeout - no data from RTSP server" messages
2. ✅ "received first RTP packet successfully" within 5 seconds of PLAY
3. ✅ Packet count increases (logged every 1000 packets)
4. ✅ No "unexpected data in stream" warnings
5. ✅ Process runs without crashes for at least 60 seconds

## Failure Indicators

- ❌ "read timeout - no data from RTSP server" (original bug persists)
- ❌ "unexpected data in stream" (byte alignment issue)
- ❌ "failed to unmarshal RTP packet" (packet corruption)
- ❌ Process exits with error

## Debugging (If Test Fails)

1. Check first 4 bytes received:
   ```bash
   grep "first_4_bytes" test_fix.log
   ```

2. Check buffer state after PLAY:
   ```bash
   grep "buffered data after PLAY" test_fix.log
   ```

3. Enable debug logging (edit main.go):
   ```go
   Level: slog.LevelDebug  // Was: slog.LevelInfo
   ```

4. Compare with ffmpeg behavior:
   ```bash
   ffmpeg -rtsp_transport tcp -i "rtsps://..." -f null - 2>&1 | grep -i rtp
   ```

## Post-Test Verification

- [ ] First packet received: _____ seconds after PLAY
- [ ] Total packets in 60s: _____ packets
- [ ] Packet rate: _____ packets/sec (expect ~30 for video, ~50 for audio)
- [ ] Zero timeouts: YES / NO
- [ ] Zero crashes: YES / NO

## Notes
- Nest cameras typically send ~30 FPS video (720p H.264)
- Audio packets arrive at ~50 Hz (AAC)
- Total expected rate: ~80 packets/sec (~4800 packets/minute)
