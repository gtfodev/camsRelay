package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
)

// LogLevel represents the logging verbosity level
type LogLevel string

const (
	LevelDebug LogLevel = "debug"
	LevelInfo  LogLevel = "info"
	LevelWarn  LogLevel = "warn"
	LevelError LogLevel = "error"
)

// DebugCategory represents specific debug categories for targeted debugging
type DebugCategory string

const (
	DebugRTP   DebugCategory = "rtp"
	DebugNAL   DebugCategory = "nal"
	DebugTrack DebugCategory = "track"
	DebugRTSP  DebugCategory = "rtsp"
	DebugWebRTC DebugCategory = "webrtc"
	DebugAll   DebugCategory = "all"
)

// Config holds logger configuration
type Config struct {
	Level           LogLevel
	Format          OutputFormat
	OutputFile      string
	EnabledCategories map[DebugCategory]bool
	mu              sync.RWMutex
}

// OutputFormat determines the log output format
type OutputFormat string

const (
	FormatJSON OutputFormat = "json"
	FormatText OutputFormat = "text"
)

// Global logger instance
var (
	defaultLogger *Logger
	once          sync.Once
)

// Logger wraps slog.Logger with category-based debugging
type Logger struct {
	*slog.Logger
	config *Config
	file   *os.File
}

// NewConfig creates a new logger configuration with defaults
func NewConfig() *Config {
	return &Config{
		Level:             LevelInfo,
		Format:            FormatText,
		OutputFile:        "",
		EnabledCategories: make(map[DebugCategory]bool),
	}
}

// ParseLevel converts a string to LogLevel
func ParseLevel(level string) (LogLevel, error) {
	switch level {
	case "debug", "DEBUG":
		return LevelDebug, nil
	case "info", "INFO":
		return LevelInfo, nil
	case "warn", "WARN", "warning", "WARNING":
		return LevelWarn, nil
	case "error", "ERROR":
		return LevelError, nil
	default:
		return "", fmt.Errorf("invalid log level: %s (must be debug, info, warn, or error)", level)
	}
}

// ParseFormat converts a string to OutputFormat
func ParseFormat(format string) (OutputFormat, error) {
	switch format {
	case "json", "JSON":
		return FormatJSON, nil
	case "text", "TEXT":
		return FormatText, nil
	default:
		return "", fmt.Errorf("invalid log format: %s (must be json or text)", format)
	}
}

// ToSlogLevel converts LogLevel to slog.Level
func (l LogLevel) ToSlogLevel() slog.Level {
	switch l {
	case LevelDebug:
		return slog.LevelDebug
	case LevelInfo:
		return slog.LevelInfo
	case LevelWarn:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// New creates a new Logger instance with the given configuration
func New(cfg *Config) (*Logger, error) {
	var writer io.Writer = os.Stdout
	var file *os.File

	// Setup output file if specified
	if cfg.OutputFile != "" {
		f, err := os.OpenFile(cfg.OutputFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("failed to open log file %s: %w", cfg.OutputFile, err)
		}
		writer = f
		file = f
	}

	// Create handler based on format
	var handler slog.Handler
	handlerOpts := &slog.HandlerOptions{
		Level: cfg.Level.ToSlogLevel(),
	}

	switch cfg.Format {
	case FormatJSON:
		handler = slog.NewJSONHandler(writer, handlerOpts)
	case FormatText:
		handler = slog.NewTextHandler(writer, handlerOpts)
	default:
		handler = slog.NewTextHandler(writer, handlerOpts)
	}

	logger := &Logger{
		Logger: slog.New(handler),
		config: cfg,
		file:   file,
	}

	return logger, nil
}

// EnableCategory enables a specific debug category
func (c *Config) EnableCategory(category DebugCategory) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if category == DebugAll {
		// Enable all categories
		c.EnabledCategories[DebugRTP] = true
		c.EnabledCategories[DebugNAL] = true
		c.EnabledCategories[DebugTrack] = true
		c.EnabledCategories[DebugRTSP] = true
		c.EnabledCategories[DebugWebRTC] = true
	} else {
		c.EnabledCategories[category] = true
	}
}

// IsCategoryEnabled checks if a debug category is enabled
func (c *Config) IsCategoryEnabled(category DebugCategory) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.EnabledCategories[category]
}

// IsDebugEnabled checks if any debug category is enabled
func (c *Config) IsDebugEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.EnabledCategories) > 0
}

// Close closes the log file if one was opened
func (l *Logger) Close() error {
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// Category-specific logging methods

// DebugRTP logs RTP packet details if RTP debugging is enabled
func (l *Logger) DebugRTP(msg string, args ...any) {
	if l.config.IsCategoryEnabled(DebugRTP) {
		args = append([]any{"category", "rtp"}, args...)
		l.Debug(msg, args...)
	}
}

// DebugNAL logs NAL unit details if NAL debugging is enabled
func (l *Logger) DebugNAL(msg string, args ...any) {
	if l.config.IsCategoryEnabled(DebugNAL) {
		args = append([]any{"category", "nal"}, args...)
		l.Debug(msg, args...)
	}
}

// DebugTrack logs track details if track debugging is enabled
func (l *Logger) DebugTrack(msg string, args ...any) {
	if l.config.IsCategoryEnabled(DebugTrack) {
		args = append([]any{"category", "track"}, args...)
		l.Debug(msg, args...)
	}
}

// DebugRTSP logs RTSP details if RTSP debugging is enabled
func (l *Logger) DebugRTSP(msg string, args ...any) {
	if l.config.IsCategoryEnabled(DebugRTSP) {
		args = append([]any{"category", "rtsp"}, args...)
		l.Debug(msg, args...)
	}
}

// DebugWebRTC logs WebRTC details if WebRTC debugging is enabled
func (l *Logger) DebugWebRTC(msg string, args ...any) {
	if l.config.IsCategoryEnabled(DebugWebRTC) {
		args = append([]any{"category", "webrtc"}, args...)
		l.Debug(msg, args...)
	}
}

// DebugRTPPacket logs detailed RTP packet information
func (l *Logger) DebugRTPPacket(seq uint16, timestamp uint32, payloadType uint8, payloadSize int) {
	if l.config.IsCategoryEnabled(DebugRTP) {
		l.Debug("RTP packet",
			"category", "rtp",
			"sequence", seq,
			"timestamp", timestamp,
			"payload_type", payloadType,
			"payload_size", payloadSize)
	}
}

// DebugRTPPayload logs raw RTP payload bytes
func (l *Logger) DebugRTPPayload(seq uint16, payload []byte) {
	if l.config.IsCategoryEnabled(DebugRTP) {
		// Log first 32 bytes of payload as hex
		maxBytes := 32
		if len(payload) < maxBytes {
			maxBytes = len(payload)
		}
		l.Debug("RTP payload",
			"category", "rtp",
			"sequence", seq,
			"payload_bytes", fmt.Sprintf("% x", payload[:maxBytes]),
			"total_size", len(payload))
	}
}

// DebugNALUnit logs NAL unit type and size
func (l *Logger) DebugNALUnit(naluType uint8, size int, fragmented bool) {
	if l.config.IsCategoryEnabled(DebugNAL) {
		naluTypeName := getNALUTypeName(naluType)
		l.Debug("NAL unit",
			"category", "nal",
			"type", naluType,
			"type_name", naluTypeName,
			"size", size,
			"fragmented", fragmented)
	}
}

// DebugNALPayload logs raw NAL unit payload bytes
func (l *Logger) DebugNALPayload(naluType uint8, payload []byte) {
	if l.config.IsCategoryEnabled(DebugNAL) {
		// Log first 64 bytes of NAL payload as hex
		maxBytes := 64
		if len(payload) < maxBytes {
			maxBytes = len(payload)
		}
		naluTypeName := getNALUTypeName(naluType)
		l.Debug("NAL payload",
			"category", "nal",
			"type", naluType,
			"type_name", naluTypeName,
			"payload_bytes", fmt.Sprintf("% x", payload[:maxBytes]),
			"total_size", len(payload))
	}
}

// WithContext adds context values to logger
func (l *Logger) WithContext(ctx context.Context) *Logger {
	return &Logger{
		Logger: l.Logger,
		config: l.config,
		file:   l.file,
	}
}

// With returns a new Logger with the given attributes
func (l *Logger) With(args ...any) *Logger {
	return &Logger{
		Logger: l.Logger.With(args...),
		config: l.config,
		file:   l.file,
	}
}

// Helper function to get NAL unit type name
func getNALUTypeName(naluType uint8) string {
	switch naluType {
	case 1:
		return "P-frame"
	case 5:
		return "IDR"
	case 6:
		return "SEI"
	case 7:
		return "SPS"
	case 8:
		return "PPS"
	case 9:
		return "AUD"
	case 28:
		return "FU-A"
	default:
		return fmt.Sprintf("unknown(%d)", naluType)
	}
}

// SetDefault sets the global default logger
func SetDefault(logger *Logger) {
	defaultLogger = logger
	slog.SetDefault(logger.Logger)
}

// Default returns the default logger, creating one if necessary
func Default() *Logger {
	once.Do(func() {
		cfg := NewConfig()
		logger, err := New(cfg)
		if err != nil {
			// Fallback to basic logger
			logger = &Logger{
				Logger: slog.Default(),
				config: cfg,
			}
		}
		defaultLogger = logger
	})
	return defaultLogger
}

// Package-level convenience functions

// Debug logs at Debug level using the default logger
func Debug(msg string, args ...any) {
	Default().Debug(msg, args...)
}

// Info logs at Info level using the default logger
func Info(msg string, args ...any) {
	Default().Info(msg, args...)
}

// Warn logs at Warn level using the default logger
func Warn(msg string, args ...any) {
	Default().Warn(msg, args...)
}

// Error logs at Error level using the default logger
func Error(msg string, args ...any) {
	Default().Error(msg, args...)
}
