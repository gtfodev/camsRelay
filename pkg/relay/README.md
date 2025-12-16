# Multi-Camera Relay Package

This package implements the complete relay infrastructure for streaming multiple Nest cameras to Cloudflare Calls via WebRTC.

## Architecture

```
MultiCameraRelay (orchestrator)
  ├─ MultiStreamManager (Nest stream lifecycle)
  │   ├─ CommandQueue (10 QPM rate limiting)
  │   └─ StreamManager per camera (auto-extension)
  │
  └─ CameraRelay per camera (media pipeline)
      ├─ RTSP client (TCP interleaved)
      ├─ RTP processors (H.264, AAC)
      └─ WebRTC bridge (Cloudflare)
```

## Components

### CameraRelay

Single camera relay handler that manages the complete pipeline:

- **RTSP Connection**: Connects to Nest camera RTSP stream
- **RTP Processing**: Depacketizes H.264 video and AAC audio
- **WebRTC Bridge**: Packetizes and sends media to Cloudflare
- **Error Handling**: Detects RTSP/WebRTC disconnects and triggers recovery

**Key Features**:
- Dedicated goroutines for packet reading, stats, and monitoring
- Atomic counters for thread-safe statistics
- Context-based cancellation for graceful shutdown
- Callbacks for disconnect events

### MultiCameraRelay

Orchestrates multiple `CameraRelay` instances:

- **Stream Monitoring**: Polls `MultiStreamManager` for stream state changes
- **Relay Lifecycle**: Creates relays when streams become `StateRunning`
- **Auto-Recovery**: Removes relays for failed streams
- **Aggregate Stats**: Provides unified view across all cameras

**Key Features**:
- Reconciliation loop (10s interval) syncs relays with stream states
- Parallel relay startup/shutdown with `sync.WaitGroup`
- Thread-safe relay map with `sync.RWMutex`
- Graceful shutdown sequence: relays → stream manager

## Usage

```go
import (
    "github.com/ethan/nest-cloudflare-relay/pkg/relay"
    "github.com/ethan/nest-cloudflare-relay/pkg/nest"
    "github.com/ethan/nest-cloudflare-relay/pkg/cloudflare"
)

// Create stream manager
streamMgr := nest.NewMultiStreamManager(
    nestClient,
    projectID,
    nest.DefaultMultiStreamConfig(),
    logger,
)

// Create multi-relay orchestrator
multiRelay := relay.NewMultiCameraRelay(
    streamMgr,
    cfClient,
    logger,
)

// Start relay (starts stream manager internally)
if err := multiRelay.Start(ctx); err != nil {
    log.Fatal(err)
}

// Start cameras with staggered initialization
cameraIDs := []string{"device1", "device2", ...}
if err := streamMgr.StartCameras(ctx, cameraIDs); err != nil {
    log.Fatal(err)
}

// Monitor status
stats := multiRelay.GetAggregateStats()
fmt.Printf("Active relays: %d, Connected: %d\n",
    stats.TotalRelays, stats.ConnectedRelays)

// Graceful shutdown
multiRelay.Stop()
```

## Lifecycle Flow

1. **Initialization**
   - `MultiCameraRelay` created with `MultiStreamManager` and Cloudflare client
   - Stream manager started (activates command queue)

2. **Camera Startup** (per camera, staggered)
   - Stream state: `StateStarting`
   - Generate RTSP stream (via queue, LOW priority)
   - Stream state: `StateRunning` on success

3. **Relay Creation**
   - Monitoring loop detects `StateRunning`
   - Create `CameraRelay` with stream URL
   - Connect RTSP → Setup tracks → Start playback
   - Create WebRTC bridge → Negotiate SDP
   - Start packet reading and forwarding

4. **Steady State**
   - RTP packets: RTSP → H.264 processor → WebRTC bridge → Cloudflare
   - Stream extensions (every 180s, HIGH priority)
   - Stats logged every 30s

5. **Error Recovery**
   - **RTSP disconnect**: Monitoring loop detects stream failure → `MultiStreamManager` regenerates
   - **WebRTC disconnect**: Relay detects state change → recreates Cloudflare session
   - **Extension failure**: Exponential backoff, degraded state after 5 failures

6. **Shutdown**
   - Cancel context → signal all goroutines
   - Stop all relays: close RTSP → wait goroutines → close WebRTC
   - Stop stream manager: stop all stream managers → stop queue

## Rate Limiting

For 20 cameras at 10 QPM limit:

- **Startup**: 12s stagger = 20 cameras in 4 minutes = 5 QPM
- **Steady state**: 20 cameras ÷ 4 minutes = 5 extensions/min
- **Total**: ~10 QPM (at limit)
- **Priority**: Extensions (HIGH) > Generates (LOW)

## Error Handling

### RTSP Disconnects
- **Detection**: `ReadPackets()` returns error
- **Callback**: `OnRTSPDisconnect()` invoked
- **Recovery**: Stream marked failed → `MultiStreamManager` regenerates with backoff

### WebRTC Disconnects
- **Detection**: Monitor loop sees state transition to "failed"/"disconnected"
- **Callback**: `OnWebRTCDisconnect()` invoked
- **Recovery**: Relay stopped → recreated in next reconciliation cycle

### Stream Expiration
- **Detection**: `MultiStreamManager` monitors TTL
- **Action**: Extension command submitted to queue (HIGH priority)
- **Failure**: Exponential backoff, degraded state after 5 failures

## Observability

### Per-Camera Statistics
```go
stats := relay.GetStats()
// CameraID, DeviceID, SessionID
// Uptime, VideoPackets, VideoFrames
// AudioPackets, AudioFrames
// WebRTCState, StreamExpiresAt
```

### Aggregate Statistics
```go
agg := multiRelay.GetAggregateStats()
// TotalRelays, ConnectedRelays, FailedRelays
// TotalVideoPackets, TotalVideoFrames
// TotalAudioPackets, TotalAudioFrames
```

### Stream Manager Statistics
```go
queueStats := streamMgr.GetQueueStats()
streamStatuses := streamMgr.GetStreamStatus()
// Per-camera state, failure counts, last errors
// Queue depth, execution counts, wait times
```

## Concurrency Patterns

### Context Hierarchy
- Parent context controls entire multi-relay lifetime
- Each `CameraRelay` has derived context for individual lifecycle
- Cancellation propagates: parent cancel → all relays stop

### Goroutine Coordination
- `sync.WaitGroup` tracks all async operations
- Goroutines check `ctx.Done()` in loops
- `defer wg.Done()` ensures proper cleanup

### Thread-Safe State
- `sync.RWMutex` protects relay map
- `atomic.Uint64` for packet/frame counters
- Read locks for stats, write locks for modifications

### Channel Usage
- No shared channels between relays (isolated pipelines)
- RTSP client uses callbacks (invoked from read goroutine)
- WebRTC bridge writes are synchronous

## Memory Management

- **Buffer Pooling**: Consider using `sync.Pool` for RTP packet buffers (not yet implemented)
- **Bounded Buffers**: RTP processors use pre-allocated 1MB buffers
- **Cleanup**: Explicit cleanup in defer statements prevents leaks

## Performance Considerations

- **Packet Processing**: Per-camera goroutines avoid contention
- **Stats Logging**: Reduced frequency (every 30s) minimizes overhead
- **Lock Granularity**: Fine-grained locking in reconciliation loop
- **Parallel Shutdown**: Relays stopped concurrently with WaitGroup

## Testing Recommendations

1. **Unit Tests**: Mock RTSP/WebRTC components, test lifecycle
2. **Integration Tests**: Single camera end-to-end with real APIs
3. **Load Tests**: 20 cameras for extended period, verify rate limiting
4. **Failure Tests**: Inject RTSP/WebRTC failures, verify recovery
5. **Shutdown Tests**: Verify no goroutine leaks with `-race` flag

## Known Limitations

1. **Audio Transcoding**: AAC → Opus not implemented (video-only)
2. **Dynamic Bitrate**: No adaptive bitrate based on network conditions
3. **PLI Requests**: No periodic Picture Loss Indication to camera
4. **Metrics Export**: No Prometheus/StatsD integration (only logs)
5. **Stream Selection**: Always uses first available stream URL

## Future Enhancements

- [ ] Audio transcoding (AAC to Opus via FFmpeg/libopus)
- [ ] Adaptive bitrate control based on Cloudflare feedback
- [ ] Metrics export (Prometheus, OpenTelemetry)
- [ ] RTCP feedback loop (PLI, NACK)
- [ ] Health check endpoints for Kubernetes
- [ ] Graceful camera add/remove without full restart
