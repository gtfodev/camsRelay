# Nest Camera Stream Management

Production-ready Go package for managing Google Nest camera RTSP streams with rate-limited multi-camera coordination.

## Features

- **Single Camera Management** - `StreamManager` for individual camera streams
- **Multi-Camera Orchestration** - `MultiStreamManager` for up to 20 cameras
- **Rate Limiting** - Smooth 10 QPM pacing with `golang.org/x/time/rate`
- **Priority Queue** - Extensions before regenerations ("Save the Living")
- **Automatic Extension** - Streams kept alive with 60s buffer before expiry
- **Error Recovery** - Exponential backoff with degraded state for persistent failures
- **Graceful Shutdown** - Context-based cancellation with WaitGroup coordination
- **Monitoring** - Queue and stream statistics for observability

---

## Quick Start

### Single Camera

```go
import "github.com/ethan/nest-cloudflare-relay/pkg/nest"

// Create client
client := nest.NewClient(clientID, clientSecret, refreshToken, logger)

// Generate stream
stream, err := client.GenerateRTSPStream(ctx, projectID, deviceID)
if err != nil {
    log.Fatal(err)
}

// Create manager (automatically handles extensions)
manager := nest.NewStreamManager(client, stream, logger)
manager.Start()
defer manager.Stop(ctx)

// Stream is now active and will be kept alive until Stop()
```

### Multiple Cameras (20 cameras)

```go
import "github.com/ethan/nest-cloudflare-relay/pkg/nest"

// Create client
client := nest.NewClient(clientID, clientSecret, refreshToken, logger)

// Configure for 20 cameras at 10 QPM
config := nest.DefaultMultiStreamConfig()

// Create multi-stream manager
manager := nest.NewMultiStreamManager(client, projectID, config, logger)
manager.Start()
defer manager.Stop()

// Start all cameras (staggered over 4 minutes)
cameraIDs := []string{"camera1", "camera2", ..., "camera20"}
if err := manager.StartCameras(ctx, cameraIDs); err != nil {
    log.Fatal(err)
}

// Monitor status
for _, status := range manager.GetStreamStatus() {
    log.Printf("Camera %s: %s", status.CameraID, status.State)
}
```

---

## Architecture

### Component Hierarchy

```
pkg/nest/
├── client.go          - Google Nest API client (OAuth, stream operations)
├── manager.go         - Single camera stream lifecycle manager
├── queue.go           - Priority queue with rate limiter
└── multi_manager.go   - Multi-camera orchestration
```

### Data Flow

```
MultiStreamManager
├── CommandQueue (rate limiter: 10 QPM, burst: 1)
│   ├── Priority Heap: CmdExtend (0) > CmdGenerate (1)
│   └── Worker Loop: processes queue with rate limiting
│
├── CameraStream[20]
│   ├── StreamManager (extension loop)
│   ├── Monitor Loop (submits extensions to queue)
│   └── Recovery Loop (handles failures)
│
└── Statistics
    ├── Queue: depth, executed, failed, avg wait time
    └── Streams: state, failures, expiry, last extension
```

---

## Core Types

### Client

Handles Google Nest SDM API authentication and operations.

```go
type Client struct {
    // OAuth credentials
    clientID     string
    clientSecret string
    refreshToken string

    // Token cache (protected by mutex)
    accessToken string
    tokenExpiry time.Time
}
```

**Methods**:
- `ListDevices(ctx, projectID)` - Enumerate cameras
- `GenerateRTSPStream(ctx, projectID, deviceID)` - Create new stream
- `ExtendRTSPStream(ctx, stream)` - Extend existing stream
- `StopRTSPStream(ctx, stream)` - Terminate stream

### StreamManager

Manages a single camera stream lifecycle with automatic extensions.

```go
type StreamManager struct {
    client *Client
    stream *RTSPStream

    extensionInterval time.Duration // Default: 60s before expiry
}
```

**Methods**:
- `Start()` - Begin extension loop
- `Stop(ctx)` - Gracefully stop stream
- `GetExpiresAt()` - When stream expires
- `GetTimeUntilExpiry()` - Time remaining

**Extension Loop**:
1. Calculate time until expiry
2. Sleep until (expiry - 60s)
3. Call `extendWithRetry()` (3 attempts, exponential backoff)
4. Repeat

### CommandQueue

Rate-limited priority queue for API commands.

```go
type CommandQueue struct {
    limiter *rate.Limiter    // 10 QPM, burst=1
    heap    ticketHeap       // Priority queue
}
```

**Methods**:
- `Start()` - Begin worker loop
- `Stop()` - Drain and shutdown
- `SubmitExtend(cameraID, fn)` - HIGH priority (0)
- `SubmitGenerate(cameraID, attempt, fn)` - LOW priority (1)
- `GetStats()` - Queue metrics

**Priority Rules**:
1. Lower priority value = higher priority (0 < 1)
2. Within same priority: FIFO (timestamp)
3. Rate limiter applied before execution

### MultiStreamManager

Orchestrates multiple camera streams with coordinated rate limiting.

```go
type MultiStreamManager struct {
    queue   *CommandQueue
    streams map[string]*CameraStream
}
```

**Methods**:
- `Start()` - Start command queue
- `Stop()` - Gracefully stop all streams
- `StartCameras(ctx, cameraIDs)` - Staggered initialization
- `GetStreamStatus()` - Per-camera state
- `GetQueueStats()` - Queue metrics

**Camera States**:
- `Starting` → Initial generation in progress
- `Running` → Active, extensions via monitor loop
- `Failed` → Exponential backoff recovery
- `Degraded` → 5+ failures, 5-minute retry interval
- `Stopped` → Intentionally stopped

---

## Configuration

### DefaultMultiStreamConfig

Tuned for 20 cameras at Google's 10 QPM limit:

```go
config := nest.DefaultMultiStreamConfig()

// Values:
// QPM: 10.0                    - Google's API quota
// StaggerInterval: 12s         - 20 cameras × 12s = 4min startup
// MaxFailures: 5               - Degrade after 5 consecutive failures
// DegradedRetry: 5min          - Retry interval when degraded
// RecoveryBaseDelay: 10s       - Exponential backoff base
```

### Custom Configuration

```go
config := nest.MultiStreamConfig{
    QPM:               20.0,            // Higher quota
    StaggerInterval:   6 * time.Second, // Faster startup
    MaxFailures:       10,              // More retries
    DegradedRetry:     2 * time.Minute, // More aggressive
    RecoveryBaseDelay: 5 * time.Second, // Faster recovery
}
```

---

## Error Handling

### Extension Failures

**404 / Stream Expired**:
1. Mark camera as `Failed`
2. Submit regeneration (LOW priority)
3. Exponential backoff: 10s, 20s, 40s, 80s, 160s

**429 / Rate Limited**:
1. Command times out (30s)
2. Recovery loop continues with backoff
3. Priority queue protects healthy streams

**Network Timeout**:
1. Context deadline (30s)
2. Does not consume API quota
3. Retry with backoff

### Degraded State

After 5 consecutive failures:
1. Camera marked `Degraded`
2. Fixed 5-minute retry interval
3. Manual intervention likely needed
4. Prevents API quota exhaustion

---

## Monitoring

### Queue Statistics

```go
stats := manager.GetQueueStats()

type QueueStats struct {
    QueueDepth    int           // Current tickets waiting
    TotalEnqueued int64         // Lifetime enqueued
    TotalExecuted int64         // Lifetime executed
    TotalFailed   int64         // Lifetime failed
    ExtendCount   int64         // Total extensions
    GenerateCount int64         // Total generations
    AvgWaitTime   time.Duration // Exponential moving average
}
```

**Alerts**:
- `QueueDepth > 10` → Backlog building up
- `TotalFailed / TotalExecuted > 0.05` → >5% failure rate
- `AvgWaitTime > 10s` → Queue congestion

### Stream Status

```go
statuses := manager.GetStreamStatus()

type StreamStatus struct {
    CameraID        string
    State           CameraState
    FailureCount    int
    LastError       error
    LastAttempt     time.Time
    LastExtension   time.Time
    StreamExpiry    time.Time
    TimeUntilExpiry time.Duration
}
```

**Alerts**:
- `State == Degraded` → Manual intervention needed
- `TimeUntilExpiry < 30s` → Extension critically late
- `FailureCount > 3` → Camera approaching degraded

---

## QPM Budget

### Steady State (20 cameras)

**Stream Lifecycle**:
- Stream lifetime: 5 minutes (300s)
- Extension threshold: 60s before expiry
- Extension frequency: ~4 minutes per camera

**QPM Breakdown**:
- Extensions: 20 ÷ 4min = **5 QPM**
- Recovery headroom: **4 QPM**
- Safety margin: **1 QPM**
- **Total: 10 QPM** (at limit)

### Startup Phase

**Staggered Timeline**:
- 12 seconds between cameras
- 20 cameras × 12s = 4 minutes
- Generation rate: 20 ÷ 4min = **5 QPM**

### Recovery Scenarios

**10 cameras fail simultaneously**:
- 10 healthy: 10 ÷ 4min = 2.5 QPM (extensions)
- 10 degraded: 10 ÷ 5min = 2 QPM (regenerations)
- **Total: 4.5 QPM** (within limit)

**Priority queue ensures extensions never starve.**

---

## Concurrency Safety

### Mutexes

**CommandQueue.mu**:
- Protects priority heap
- Short critical sections (O(log n))

**MultiStreamManager.mu**:
- RWMutex for `streams` map
- `RLock()` for reads, `Lock()` for writes

**Client.mu**:
- RWMutex for OAuth token cache
- `RLock()` for token read, `Lock()` for refresh

### Channels

**CommandTicket.Response**:
- Buffered (size 1)
- Caller blocks until command executes
- Closed after result sent

### Context Cancellation

**Hierarchy**:
- Parent context controls manager lifetime
- Derived contexts for individual API calls
- `WithTimeout` for network operations

### Goroutine Lifecycle

**Per Manager**:
- 1 × command queue worker
- 20 × camera startup goroutines
- 20 × stream monitor loops
- N × recovery loops (on-demand)

**Shutdown**:
1. Cancel parent context
2. Wait for all goroutines (`wg.Wait()`)
3. Stop stream managers (parallel with timeout)
4. Drain command queue

---

## Testing

### Unit Tests

```bash
go test ./pkg/nest/...
```

**Coverage**:
- Priority queue ordering
- Rate limiting behavior
- State transitions
- Error handling
- Graceful shutdown

### Integration Tests

**Single Camera**:
```bash
go run examples/multi_camera_example.go
# Set GOOGLE_* environment variables first
```

**Load Test**:
- Run 20 cameras for 24 hours
- Monitor API quota usage (≤10 QPM)
- Check extension success rate (>99%)
- Verify memory stability

---

## Performance

### Memory

**Per Camera**:
- `CameraStream`: ~200 bytes
- `StreamManager`: ~1 KB
- **Total for 20 cameras: ~24 KB**

**Command Queue**:
- Heap: ~1 KB (typically 1-5 tickets)
- **Overall: <50 KB**

### CPU

**Steady State**:
- Worker loop: 100ms ticker
- Monitor loops: 30s ticker
- API calls: blocking I/O
- **Overall: <1% CPU**

### Latency

**Extension Latency**:
- Queue wait: 0-6s (avg 3s at 10 QPM)
- API call: 500-2000ms
- **Total: 500ms - 8s** (within 60s buffer)

---

## Examples

### Basic Usage

See `/examples/multi_camera_example.go` for complete working example.

### Custom Logger

```go
import "log/slog"

logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
    Level: slog.LevelInfo,
}))

client := nest.NewClient(clientID, clientSecret, refreshToken, logger)
```

### Graceful Shutdown

```go
sigChan := make(chan os.Signal, 1)
signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

<-sigChan
log.Println("Shutting down...")

ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

if err := manager.Stop(); err != nil {
    log.Printf("Error during shutdown: %v", err)
}
```

---

## Dependencies

```
golang.org/x/time/rate v0.14.0  - Rate limiting
log/slog                         - Structured logging
container/heap                   - Priority queue
context                          - Cancellation
sync                             - Concurrency primitives
```

---

## Documentation

- `/PHASE3_IMPLEMENTATION.md` - Complete technical specification
- `/INTEGRATION_GUIDE.md` - Migration and integration patterns
- `/examples/multi_camera_example.go` - Working example with QPM analysis

---

## License

See repository LICENSE file.

---

## Support

For issues, questions, or contributions, please refer to the main repository documentation.

---

**Version**: 1.0.0
**Status**: Production Ready
**Last Updated**: 2025-12-16
