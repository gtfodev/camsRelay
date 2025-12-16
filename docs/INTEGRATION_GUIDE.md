# Integration Guide: Adding Queue to Existing StreamManager

This guide shows how to optionally integrate the `CommandQueue` with the existing `StreamManager` to route all API calls through the priority queue.

## Current Architecture (Working, No Changes Required)

```
StreamManager
├── extensionLoop() - timing logic
└── extendWithRetry() - calls client.ExtendRTSPStream() directly
```

**This continues to work perfectly for single-camera scenarios.**

## Enhanced Architecture (For Multi-Camera)

```
MultiStreamManager
├── CommandQueue (rate limiter + priority)
├── CameraStream[20]
│   └── StreamManager (uses queue implicitly)
└── Monitor loops (submit to queue)
```

**For 20 cameras, use `MultiStreamManager` which handles everything.**

---

## Option 1: Use MultiStreamManager (Recommended)

**When to use**: You have multiple cameras (2-20) and need coordinated rate limiting.

**Advantages**:
- No changes to existing `StreamManager` code
- Centralized rate limiting across all cameras
- Priority queue prevents death spiral
- Automatic staggered startup

**Code**:
```go
// Replace this:
manager := nest.NewStreamManager(client, stream, logger)
manager.Start()

// With this:
config := nest.DefaultMultiStreamConfig()
multiManager := nest.NewMultiStreamManager(client, projectID, config, logger)
multiManager.Start()
multiManager.StartCameras(ctx, cameraIDs)
```

---

## Option 2: Keep Existing Code (Single Camera)

**When to use**: You only have 1 camera or don't need rate limiting.

**Advantages**:
- Simpler code path
- No queue overhead
- Direct API calls

**Code** (no changes needed):
```go
// This continues to work perfectly
manager := nest.NewStreamManager(client, stream, logger)
manager.Start()

// Extensions happen automatically via extensionLoop
// API calls go directly to client.ExtendRTSPStream()
```

---

## Option 3: Hybrid Approach (Advanced)

**When to use**: You want to use `StreamManager` but with centralized rate limiting.

**Implementation**: Modify `StreamManager.extendWithRetry()` to use a shared queue.

### Step 1: Add Queue to StreamManager

```go
// In pkg/nest/manager.go
type StreamManager struct {
    client *Client
    stream *RTSPStream
    logger *slog.Logger
    queue  *CommandQueue // NEW: optional queue

    ctx    context.Context
    cancel context.CancelFunc
    wg     sync.WaitGroup

    extensionInterval time.Duration
}

// Add new constructor with queue
func NewStreamManagerWithQueue(
    client *Client,
    stream *RTSPStream,
    queue *CommandQueue,
    logger *slog.Logger,
) *StreamManager {
    ctx, cancel := context.WithCancel(context.Background())

    return &StreamManager{
        client:            client,
        stream:            stream,
        queue:             queue, // NEW
        logger:            logger,
        ctx:               ctx,
        cancel:            cancel,
        extensionInterval: 60 * time.Second,
    }
}
```

### Step 2: Modify extendWithRetry

```go
// In pkg/nest/manager.go
func (m *StreamManager) extendWithRetry() error {
    // If queue is set, use it for rate limiting and priority
    if m.queue != nil {
        return m.queue.SubmitExtend(m.stream.DeviceID, func() error {
            ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
            defer cancel()
            return m.client.ExtendRTSPStream(ctx, m.stream)
        })
    }

    // Otherwise, use existing logic (local retry)
    const maxRetries = 3
    backoff := 1 * time.Second

    for attempt := 0; attempt < maxRetries; attempt++ {
        ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
        err := m.client.ExtendRTSPStream(ctx, m.stream)
        cancel()

        if err == nil {
            m.logger.Info("stream extended successfully",
                "device_id", m.stream.DeviceID,
                "new_expiry", m.stream.ExpiresAt.Format(time.RFC3339),
                "attempt", attempt+1)
            return nil
        }

        m.logger.Warn("stream extension attempt failed",
            "device_id", m.stream.DeviceID,
            "attempt", attempt+1,
            "max_retries", maxRetries,
            "error", err)

        if attempt < maxRetries-1 {
            select {
            case <-m.ctx.Done():
                return m.ctx.Err()
            case <-time.After(backoff):
                backoff *= 2
            }
        }
    }

    return fmt.Errorf("max retries exceeded for stream extension")
}
```

### Step 3: Usage

```go
// Create shared queue
queue := nest.NewCommandQueue(10.0, logger) // 10 QPM
queue.Start()

// Create managers with queue
manager1 := nest.NewStreamManagerWithQueue(client, stream1, queue, logger1)
manager2 := nest.NewStreamManagerWithQueue(client, stream2, queue, logger2)

manager1.Start()
manager2.Start()

// Extensions from both managers now go through the same queue
```

**Advantages**:
- Reuses existing `StreamManager` extension logic
- Centralized rate limiting across multiple managers
- Minimal code changes

**Disadvantages**:
- Requires modifying `manager.go` (breaks simplicity)
- `MultiStreamManager` already does this better

---

## Decision Matrix

| Scenario | Recommended Approach | Rationale |
|----------|---------------------|-----------|
| 1 camera | Option 2 (existing) | Simplest, no queue overhead |
| 2-5 cameras | Option 1 (MultiStreamManager) | Best coordination, minimal complexity |
| 6-20 cameras | Option 1 (MultiStreamManager) | Essential for rate limiting |
| >20 cameras | Custom (federation) | Exceeds 10 QPM budget |
| Mixed (some with queue, some without) | Option 3 (hybrid) | Flexibility for gradual migration |

---

## Migration Path

### From Single Camera to Multi-Camera

**Before** (single camera):
```go
client := nest.NewClient(clientID, clientSecret, refreshToken, logger)
stream, err := client.GenerateRTSPStream(ctx, projectID, deviceID)
manager := nest.NewStreamManager(client, stream, logger)
manager.Start()
```

**After** (20 cameras):
```go
client := nest.NewClient(clientID, clientSecret, refreshToken, logger)
config := nest.DefaultMultiStreamConfig()
multiManager := nest.NewMultiStreamManager(client, projectID, config, logger)
multiManager.Start()

cameraIDs := []string{"camera1", "camera2", ..., "camera20"}
multiManager.StartCameras(ctx, cameraIDs)
```

**Changes**:
- Remove manual `GenerateRTSPStream()` call (handled by `MultiStreamManager`)
- Remove manual `StreamManager` creation (managed internally)
- Add camera ID list
- Use `StartCameras()` instead of individual starts

---

## Testing Strategy

### Test 1: Verify Backward Compatibility

Ensure existing single-camera code still works:

```go
func TestSingleCameraBackwardCompat(t *testing.T) {
    client := nest.NewClient(clientID, clientSecret, refreshToken, logger)
    stream, err := client.GenerateRTSPStream(ctx, projectID, deviceID)
    require.NoError(t, err)

    manager := nest.NewStreamManager(client, stream, logger)
    manager.Start()
    defer manager.Stop(ctx)

    // Wait for at least one extension cycle
    time.Sleep(5 * time.Minute)

    // Verify stream is still alive
    assert.True(t, manager.GetTimeUntilExpiry() > 60*time.Second)
}
```

### Test 2: Verify Multi-Camera Coordination

Ensure priority queue works correctly:

```go
func TestMultiCameraCoordination(t *testing.T) {
    client := nest.NewClient(clientID, clientSecret, refreshToken, logger)
    config := nest.DefaultMultiStreamConfig()
    config.QPM = 10.0

    multiManager := nest.NewMultiStreamManager(client, projectID, config, logger)
    multiManager.Start()
    defer multiManager.Stop()

    cameraIDs := []string{"cam1", "cam2", "cam3"}
    err := multiManager.StartCameras(ctx, cameraIDs)
    require.NoError(t, err)

    // Wait for startup to complete
    time.Sleep(1 * time.Minute)

    // Verify all cameras are running
    statuses := multiManager.GetStreamStatus()
    for _, status := range statuses {
        assert.Equal(t, nest.StateRunning, status.State)
    }

    // Verify rate limiting
    stats := multiManager.GetQueueStats()
    qpm := float64(stats.TotalExecuted) / time.Since(startTime).Minutes()
    assert.LessOrEqual(t, qpm, 11.0) // Allow 10% margin
}
```

### Test 3: Verify Priority Ordering

Ensure extensions take priority over generations:

```go
func TestPriorityQueue(t *testing.T) {
    queue := nest.NewCommandQueue(1.0, logger) // 1 QPM for testing
    queue.Start()
    defer queue.Stop()

    var executionOrder []string
    mu := sync.Mutex{}

    // Submit LOW priority (generate)
    go queue.SubmitGenerate("cam1", 0, func() error {
        mu.Lock()
        executionOrder = append(executionOrder, "generate")
        mu.Unlock()
        return nil
    })

    time.Sleep(100 * time.Millisecond)

    // Submit HIGH priority (extend)
    go queue.SubmitExtend("cam2", func() error {
        mu.Lock()
        executionOrder = append(executionOrder, "extend")
        mu.Unlock()
        return nil
    })

    time.Sleep(2 * time.Minute) // Allow both to execute

    mu.Lock()
    defer mu.Unlock()
    // Extend should execute first despite being submitted second
    assert.Equal(t, []string{"extend", "generate"}, executionOrder)
}
```

---

## Troubleshooting

### Issue: Queue depth keeps growing

**Symptoms**: `GetQueueStats().QueueDepth` increases over time

**Causes**:
1. API calls taking too long (>30s timeout)
2. Rate limit too low for number of cameras
3. Worker loop not processing fast enough

**Solutions**:
1. Check API latency: `stats.AvgWaitTime`
2. Increase QPM if you have higher quota
3. Verify worker loop is running: check logs for "command executed"

### Issue: Extensions failing with 404

**Symptoms**: Cameras enter `StateFailed`, logs show "stream expired"

**Causes**:
1. Extension happening too late (stream already expired)
2. Recovery loop not regenerating fast enough

**Solutions**:
1. Reduce `extensionInterval` in `StreamManager` (default 60s)
2. Check queue depth - extensions may be delayed
3. Verify time sync on server (clock drift can cause issues)

### Issue: All cameras marked degraded

**Symptoms**: All cameras in `StateDegraded` after startup

**Causes**:
1. Google API credentials invalid or expired
2. Network connectivity issues
3. Rate limiting too aggressive (429 responses)

**Solutions**:
1. Verify OAuth credentials: test with single camera first
2. Check network: `curl https://smartdevicemanagement.googleapis.com`
3. Review logs for 429 errors, increase backoff if needed

---

## Summary

**For most users**: Use `MultiStreamManager` (Option 1). It handles everything.

**For single camera**: Keep existing `StreamManager` (Option 2). No changes needed.

**For gradual migration**: Consider hybrid approach (Option 3), but `MultiStreamManager` is cleaner.

**Key files**:
- `/home/ethan/cams/pkg/nest/queue.go` - Priority queue + rate limiter
- `/home/ethan/cams/pkg/nest/multi_manager.go` - Multi-camera orchestration
- `/home/ethan/cams/pkg/nest/manager.go` - Existing single-camera manager (unchanged)
- `/home/ethan/cams/examples/multi_camera_example.go` - Complete working example

**No breaking changes**: Existing code continues to work. New functionality is additive.
