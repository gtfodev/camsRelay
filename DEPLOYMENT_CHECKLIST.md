# Leaky Bucket Pacer - Deployment Checklist

## Pre-Deployment Verification

### Build & Test
- [x] Go build successful (no compilation errors)
- [x] Go vet clean (no static analysis warnings)
- [ ] Run integration tests
- [ ] Monitor logs for pacer statistics during local testing

### Code Review
- [x] Pacer implements report Section 8.2 architecture
- [x] Timestamp passthrough preserved (90kHz video, 48kHz audio)
- [x] Sequence number management unchanged
- [x] Catch-up mode implemented (1.1x drain speed)
- [x] Safety caps in place (max 200ms delay)
- [x] Comprehensive logging added

### Configuration Validation
- [x] Video clock rate: 90000 Hz (H.264 standard)
- [x] Audio clock rate: 48000 Hz (Opus standard)
- [x] Channel buffer size: 10 packets
- [x] Catch-up threshold: 5 packets
- [x] Catch-up multiplier: 1.1x
- [x] Max delay cap: 200ms

## Deployment Steps

### 1. Staging Deployment

```bash
# Build for production
go build -o bin/relay ./cmd/relay

# Deploy to staging
scp bin/relay staging:/opt/nest-relay/

# Restart service
ssh staging "systemctl restart nest-relay"
```

### 2. Monitor Pacer Statistics

Watch for these log lines every 30 seconds:

```json
{
  "level": "info",
  "msg": "pacer statistics",
  "video_packets_sent": 9000,
  "bursts_absorbed": 12,
  "catchup_events": 3,
  "avg_video_delay_ms": 31,
  "video_queue_depth": 2
}
```

**Expected values:**
- `bursts_absorbed`: <20 per minute
- `catchup_events`: <5 per minute
- `avg_video_delay_ms`: 30-35ms (for 30fps)
- `video_queue_depth`: <5 most of time

### 3. Browser WebRTC Statistics

Monitor in Chrome DevTools (chrome://webrtc-internals):

**Before Pacer:**
```
inbound-rtp (video):
  jitter: 0.08-0.15s (80-150ms) - HIGH
  packetsLost: 10-50 per second
  jitterBufferDelay: 0.3-0.8s (growing over time)
```

**Expected After Pacer:**
```
inbound-rtp (video):
  jitter: 0.01-0.03s (10-30ms) - LOW
  packetsLost: 0-2 per second
  jitterBufferDelay: 0.05-0.1s (stable)
```

### 4. Issue Resolution Metrics

| Issue | Before | Target After | How to Measure |
|-------|--------|--------------|----------------|
| Streams don't load | 50% success | 95% success | Monitor initial WebRTC connection success rate |
| Frontend stops | 30% of sessions | <1% of sessions | Track JavaScript errors + rendering freezes |
| Stuttering | 40 events/hour | <5 events/hour | Monitor jitter buffer expansion events |

### 5. Rollback Plan

If issues occur:

```bash
# Revert to previous version
ssh staging "systemctl stop nest-relay"
scp bin/relay.backup staging:/opt/nest-relay/relay
ssh staging "systemctl start nest-relay"
```

**Rollback triggers:**
- `bursts_absorbed` > 100/min (persistent)
- `video_queue_depth` > 9 (approaching overflow)
- CPU usage > 20% (pacer overhead)
- Memory leak detected
- WebRTC jitter increases instead of decreases

## Post-Deployment Validation

### Immediate (First Hour)

- [ ] All existing streams remain stable
- [ ] New stream connections succeed
- [ ] No pacer goroutine leaks (check goroutine count)
- [ ] No memory leaks (check RSS)
- [ ] Pacer statistics appear in logs

### Short-term (First Day)

- [ ] Monitor burst absorption frequency
- [ ] Monitor catch-up event frequency
- [ ] Check for any timestamp anomaly warnings
- [ ] Validate browser jitter buffer metrics
- [ ] Confirm no increase in packet loss

### Medium-term (First Week)

- [ ] Compare issue #1 frequency (week over week)
- [ ] Compare issue #2 frequency (week over week)
- [ ] Compare issue #3 frequency (week over week)
- [ ] Gather user feedback on stream quality
- [ ] Review CloudWatch/monitoring dashboards

## Performance Tuning

If metrics are outside expected ranges:

### High Burst Absorption (>50/min)

**Possible causes:**
- Network congestion
- RTSP server throttling
- TCP retransmissions

**Tuning:**
```go
// Increase buffer to absorb larger bursts
videoChan: make(chan *PacedPacket, 20)  // Was 10
```

### Frequent Catch-up Events (>20/min)

**Possible causes:**
- Persistent slow consumer
- Cloudflare rate limiting
- Browser cannot keep up

**Tuning:**
```go
// More aggressive catch-up
catchupSpeedMultiplier = 1.2  // Was 1.1
catchupThreshold = 3          // Was 5
```

### High Average Delay (>50ms for 30fps)

**Possible causes:**
- Clock drift
- Timestamp calculation error
- Accumulated delays

**Investigation:**
- Check for "capping excessive delay" warnings
- Verify source timestamps are monotonic
- Compare wall clock vs RTP timestamp deltas

## Success Criteria

Consider deployment successful if after 7 days:

1. **Stream Load Success Rate** ≥ 90%
2. **Frontend Stop Incidents** < 5 per day
3. **Stuttering Reports** < 10 per day
4. **Pacer Queue Overflow** = 0
5. **Average Jitter** < 50ms
6. **CPU Overhead** < 5%
7. **Memory Overhead** < 10MB per camera

## Rollout Strategy

### Phase 1: Single Camera (Day 1)
- Deploy to staging
- Monitor one camera for 24 hours
- Validate all metrics

### Phase 2: Small Group (Day 2-3)
- Enable for 5-10 cameras
- A/B test vs old architecture
- Compare metrics side-by-side

### Phase 3: Gradual Rollout (Day 4-7)
- 25% of cameras
- 50% of cameras
- 75% of cameras
- 100% of cameras

### Phase 4: Production (Week 2+)
- Full production deployment
- Continuous monitoring
- Iterate on tuning parameters

## Emergency Contacts

**If critical issues occur:**

1. **Check logs:** `journalctl -u nest-relay -f`
2. **Monitor metrics:** CloudWatch dashboard
3. **Review WebRTC stats:** chrome://webrtc-internals
4. **Escalate if:** Packet loss > 10%, CPU > 50%, Memory leak detected

---

**Deployment Owner:** _____________
**Deployment Date:** _____________
**Signoff:** _____________
