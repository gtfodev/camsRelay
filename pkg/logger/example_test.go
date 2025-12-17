package logger_test

import (
	"fmt"
	"os"

	"github.com/ethan/nest-cloudflare-relay/pkg/logger"
)

// Example showing basic logger usage
func ExampleLogger_basic() {
	// Create logger with default config
	cfg := logger.NewConfig()
	cfg.Level = logger.LevelInfo
	cfg.Format = logger.FormatText

	log, err := logger.New(cfg)
	if err != nil {
		panic(err)
	}
	defer log.Close()

	// Basic logging
	log.Info("application started", "version", "1.0.0")
	log.Warn("deprecated API used", "endpoint", "/v1/users")
	log.Error("failed to connect", "error", "connection timeout")
}

// Example showing debug category usage
func ExampleLogger_categories() {
	cfg := logger.NewConfig()
	cfg.Level = logger.LevelDebug
	cfg.EnableCategory(logger.DebugRTP)
	cfg.EnableCategory(logger.DebugNAL)

	log, err := logger.New(cfg)
	if err != nil {
		panic(err)
	}
	defer log.Close()

	// RTP debugging (only logged if DebugRTP enabled)
	log.DebugRTPPacket(12345, 90000, 96, 1200)

	// NAL debugging (only logged if DebugNAL enabled)
	log.DebugNALUnit(7, 28, false) // SPS

	// Generic category logging
	log.DebugRTP("packet received", "seq", 12345)
	log.DebugNAL("keyframe detected", "size", 15234)
}

// Example showing command-line flags integration
func ExampleFlags() {
	// In main.go:
	// import (
	//     "flag"
	//     "github.com/ethan/nest-cloudflare-relay/pkg/logger"
	// )
	//
	// fs := flag.NewFlagSet("myapp", flag.ExitOnError)
	// logFlags := logger.RegisterFlags(fs)
	// fs.Parse(os.Args[1:])
	//
	// logConfig, _ := logFlags.ToConfig()
	// log, _ := logger.New(logConfig)
	// defer log.Close()

	fmt.Println("See cmd/relay/main.go for complete example")
}

// Example showing JSON format output
func ExampleLogger_json() {
	cfg := logger.NewConfig()
	cfg.Level = logger.LevelInfo
	cfg.Format = logger.FormatJSON
	cfg.OutputFile = "app.json"

	log, err := logger.New(cfg)
	if err != nil {
		panic(err)
	}
	defer log.Close()
	defer os.Remove("app.json") // Cleanup

	log.Info("user logged in",
		"user_id", "12345",
		"ip", "192.168.1.1",
		"duration_ms", 250)

	// Output will be in JSON format:
	// {"time":"...","level":"INFO","msg":"user logged in","user_id":"12345","ip":"192.168.1.1","duration_ms":250}
}

// Example showing conditional debug logging
func ExampleLogger_conditional() {
	cfg := logger.NewConfig()
	cfg.EnableCategory(logger.DebugNAL)

	log, err := logger.New(cfg)
	if err != nil {
		panic(err)
	}
	defer log.Close()

	// This will only execute if DebugNAL is enabled
	// No performance overhead if disabled
	payload := make([]byte, 1024)
	log.DebugNALPayload(7, payload) // Only logs first 64 bytes

	// Category methods automatically check if enabled
	// No manual check needed - zero cost if disabled
	log.DebugRTP("packet received", "seq", 12345)
}

func computeExpensiveStats() string {
	return "expensive computation result"
}
