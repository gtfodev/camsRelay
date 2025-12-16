package nest

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// CommandType defines the priority of API commands
type CommandType int

const (
	CmdExtend   CommandType = iota // Priority 0 (HIGH) - keep streams alive
	CmdGenerate                    // Priority 1 (LOW) - stream recovery
)

// String returns human-readable command type
func (c CommandType) String() string {
	switch c {
	case CmdExtend:
		return "extend"
	case CmdGenerate:
		return "generate"
	default:
		return "unknown"
	}
}

// CommandTicket represents a queued API command with priority and response channel
type CommandTicket struct {
	Type       CommandType
	CameraID   string
	Attempt    int           // Retry attempt number (for backoff calculation)
	Timestamp  time.Time     // When ticket was created
	Response   chan error    // Caller blocks on this until command executes
	ExecuteFn  func() error  // Function to execute the actual command
	priority   int           // Internal priority value for heap
	index      int           // Internal heap index
}

// ticketHeap implements heap.Interface for priority queue
type ticketHeap []*CommandTicket

func (h ticketHeap) Len() int { return len(h) }

func (h ticketHeap) Less(i, j int) bool {
	// Lower priority value = higher priority (0 < 1)
	if h[i].priority != h[j].priority {
		return h[i].priority < h[j].priority
	}
	// Within same priority, FIFO (earlier timestamp first)
	return h[i].Timestamp.Before(h[j].Timestamp)
}

func (h ticketHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *ticketHeap) Push(x interface{}) {
	n := len(*h)
	ticket := x.(*CommandTicket)
	ticket.index = n
	*h = append(*h, ticket)
}

func (h *ticketHeap) Pop() interface{} {
	old := *h
	n := len(old)
	ticket := old[n-1]
	old[n-1] = nil     // Avoid memory leak
	ticket.index = -1  // Mark as removed
	*h = old[0 : n-1]
	return ticket
}

// CommandQueue coordinates all Nest API calls with rate limiting and priority
type CommandQueue struct {
	logger  *slog.Logger
	limiter *rate.Limiter

	mu     sync.Mutex
	heap   ticketHeap
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Metrics
	stats struct {
		mu             sync.RWMutex
		totalEnqueued  int64
		totalExecuted  int64
		totalFailed    int64
		extendCount    int64
		generateCount  int64
		avgWaitTime    time.Duration
	}
}

// NewCommandQueue creates a centralized command queue with rate limiting
// qpm: queries per minute (e.g., 10 for Google's limit)
func NewCommandQueue(qpm float64, logger *slog.Logger) *CommandQueue {
	ctx, cancel := context.WithCancel(context.Background())

	// Convert QPM to queries per second with burst=1 (no bursting)
	qps := rate.Limit(qpm / 60.0)

	cq := &CommandQueue{
		logger:  logger,
		limiter: rate.NewLimiter(qps, 1), // Smooth pacing, no bursts
		ctx:     ctx,
		cancel:  cancel,
		heap:    make(ticketHeap, 0),
	}

	heap.Init(&cq.heap)

	logger.Info("command queue initialized",
		"qpm", qpm,
		"qps", float64(qps),
		"burst", 1)

	return cq
}

// Start begins processing the command queue
func (cq *CommandQueue) Start() {
	cq.wg.Add(1)
	go cq.workerLoop()
	cq.logger.Info("command queue worker started")
}

// Stop gracefully shuts down the queue, rejecting pending commands
func (cq *CommandQueue) Stop() error {
	cq.logger.Info("stopping command queue")

	cq.cancel()
	cq.wg.Wait()

	// Drain remaining tickets with cancellation error
	cq.mu.Lock()
	remaining := len(cq.heap)
	for cq.heap.Len() > 0 {
		ticket := heap.Pop(&cq.heap).(*CommandTicket)
		select {
		case ticket.Response <- context.Canceled:
		default:
		}
		close(ticket.Response)
	}
	cq.mu.Unlock()

	cq.logger.Info("command queue stopped", "drained_tickets", remaining)
	return nil
}

// SubmitExtend submits a stream extension command (HIGH priority)
func (cq *CommandQueue) SubmitExtend(cameraID string, executeFn func() error) error {
	return cq.submit(CmdExtend, cameraID, 0, executeFn)
}

// SubmitGenerate submits a stream generation command (LOW priority)
func (cq *CommandQueue) SubmitGenerate(cameraID string, attempt int, executeFn func() error) error {
	return cq.submit(CmdGenerate, cameraID, attempt, executeFn)
}

// submit enqueues a command ticket and waits for execution
func (cq *CommandQueue) submit(cmdType CommandType, cameraID string, attempt int, executeFn func() error) error {
	ticket := &CommandTicket{
		Type:      cmdType,
		CameraID:  cameraID,
		Attempt:   attempt,
		Timestamp: time.Now(),
		Response:  make(chan error, 1),
		ExecuteFn: executeFn,
		priority:  int(cmdType), // Map enum to heap priority
	}

	cq.mu.Lock()
	heap.Push(&cq.heap, ticket)
	queueDepth := cq.heap.Len()
	cq.mu.Unlock()

	cq.updateStats(func() {
		cq.stats.totalEnqueued++
		if cmdType == CmdExtend {
			cq.stats.extendCount++
		} else {
			cq.stats.generateCount++
		}
	})

	cq.logger.Debug("command enqueued",
		"type", cmdType.String(),
		"camera_id", cameraID,
		"attempt", attempt,
		"queue_depth", queueDepth)

	// Block until command executes or queue shuts down
	select {
	case err := <-ticket.Response:
		waitTime := time.Since(ticket.Timestamp)
		cq.updateStats(func() {
			// Update rolling average wait time
			if cq.stats.totalExecuted == 0 {
				cq.stats.avgWaitTime = waitTime
			} else {
				// Exponential moving average
				cq.stats.avgWaitTime = (cq.stats.avgWaitTime*9 + waitTime) / 10
			}
		})
		return err
	case <-cq.ctx.Done():
		return context.Canceled
	}
}

// workerLoop processes commands from the priority queue with rate limiting
func (cq *CommandQueue) workerLoop() {
	defer cq.wg.Done()

	ticker := time.NewTicker(100 * time.Millisecond) // Check queue every 100ms
	defer ticker.Stop()

	for {
		select {
		case <-cq.ctx.Done():
			return

		case <-ticker.C:
			cq.processNextCommand()
		}
	}
}

// processNextCommand pops highest priority ticket and executes with rate limiting
func (cq *CommandQueue) processNextCommand() {
	cq.mu.Lock()
	if cq.heap.Len() == 0 {
		cq.mu.Unlock()
		return
	}

	ticket := heap.Pop(&cq.heap).(*CommandTicket)
	queueDepth := cq.heap.Len()
	cq.mu.Unlock()

	// Apply rate limiting BEFORE execution
	if err := cq.limiter.Wait(cq.ctx); err != nil {
		// Context canceled during rate limit wait
		ticket.Response <- err
		close(ticket.Response)
		return
	}

	// Execute the command
	executeStart := time.Now()
	err := cq.executeCommand(ticket)
	executeDuration := time.Since(executeStart)

	cq.updateStats(func() {
		cq.stats.totalExecuted++
		if err != nil {
			cq.stats.totalFailed++
		}
	})

	cq.logger.Info("command executed",
		"type", ticket.Type.String(),
		"camera_id", ticket.CameraID,
		"attempt", ticket.Attempt,
		"duration_ms", executeDuration.Milliseconds(),
		"queue_depth", queueDepth,
		"success", err == nil,
		"error", err)

	// Send result back to caller
	ticket.Response <- err
	close(ticket.Response)
}

// executeCommand runs the ticket's execute function with timeout
func (cq *CommandQueue) executeCommand(ticket *CommandTicket) error {
	if ticket.ExecuteFn == nil {
		return errors.New("execute function is nil")
	}

	// Create timeout context for individual command execution
	ctx, cancel := context.WithTimeout(cq.ctx, 30*time.Second)
	defer cancel()

	// Execute in goroutine to respect timeout
	errChan := make(chan error, 1)
	go func() {
		errChan <- ticket.ExecuteFn()
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return fmt.Errorf("command timeout after 30s: %w", ctx.Err())
	}
}

// GetStats returns current queue statistics
func (cq *CommandQueue) GetStats() QueueStats {
	cq.mu.Lock()
	queueDepth := cq.heap.Len()
	cq.mu.Unlock()

	cq.stats.mu.RLock()
	defer cq.stats.mu.RUnlock()

	return QueueStats{
		QueueDepth:    queueDepth,
		TotalEnqueued: cq.stats.totalEnqueued,
		TotalExecuted: cq.stats.totalExecuted,
		TotalFailed:   cq.stats.totalFailed,
		ExtendCount:   cq.stats.extendCount,
		GenerateCount: cq.stats.generateCount,
		AvgWaitTime:   cq.stats.avgWaitTime,
	}
}

// QueueStats contains command queue metrics
type QueueStats struct {
	QueueDepth    int
	TotalEnqueued int64
	TotalExecuted int64
	TotalFailed   int64
	ExtendCount   int64
	GenerateCount int64
	AvgWaitTime   time.Duration
}

// updateStats safely updates internal stats
func (cq *CommandQueue) updateStats(fn func()) {
	cq.stats.mu.Lock()
	defer cq.stats.mu.Unlock()
	fn()
}
