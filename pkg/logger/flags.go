package logger

import (
	"flag"
	"fmt"
	"strings"
)

// Flags holds all logging-related command-line flags
type Flags struct {
	LogLevel       string
	LogFormat      string
	LogFile        string
	DebugRTP       bool
	DebugNAL       bool
	DebugTrack     bool
	DebugRTSP      bool
	DebugWebRTC    bool
	DebugAll       bool
}

// RegisterFlags registers logging flags with the given FlagSet
func RegisterFlags(fs *flag.FlagSet) *Flags {
	f := &Flags{}

	fs.StringVar(&f.LogLevel, "log-level", "info",
		"Log level: debug, info, warn, error")
	fs.StringVar(&f.LogLevel, "l", "info",
		"Log level (shorthand)")

	fs.StringVar(&f.LogFormat, "log-format", "text",
		"Log output format: text, json")

	fs.StringVar(&f.LogFile, "log-file", "",
		"Log output file path (default: stdout)")
	fs.StringVar(&f.LogFile, "o", "",
		"Log output file path (shorthand)")

	// Debug category flags
	fs.BoolVar(&f.DebugRTP, "debug-rtp", false,
		"Enable detailed RTP packet debugging (sequence, timestamp, payload)")
	fs.BoolVar(&f.DebugNAL, "debug-nal", false,
		"Enable detailed NAL unit debugging (type, size, raw bytes)")
	fs.BoolVar(&f.DebugTrack, "debug-track", false,
		"Enable track status debugging (WebRTC tracks, RTSP tracks)")
	fs.BoolVar(&f.DebugRTSP, "debug-rtsp", false,
		"Enable RTSP protocol debugging")
	fs.BoolVar(&f.DebugWebRTC, "debug-webrtc", false,
		"Enable WebRTC debugging (ICE, SDP, connection state)")
	fs.BoolVar(&f.DebugAll, "debug-all", false,
		"Enable all debug categories")

	return f
}

// ToConfig converts Flags to a logger Config
func (f *Flags) ToConfig() (*Config, error) {
	cfg := NewConfig()

	// Parse log level
	level, err := ParseLevel(f.LogLevel)
	if err != nil {
		return nil, err
	}
	cfg.Level = level

	// Parse format
	format, err := ParseFormat(f.LogFormat)
	if err != nil {
		return nil, err
	}
	cfg.Format = format

	// Set output file
	cfg.OutputFile = f.LogFile

	// Enable debug categories
	if f.DebugAll {
		cfg.EnableCategory(DebugAll)
		// Force debug level when any debug category is enabled
		cfg.Level = LevelDebug
	} else {
		if f.DebugRTP {
			cfg.EnableCategory(DebugRTP)
			cfg.Level = LevelDebug
		}
		if f.DebugNAL {
			cfg.EnableCategory(DebugNAL)
			cfg.Level = LevelDebug
		}
		if f.DebugTrack {
			cfg.EnableCategory(DebugTrack)
			cfg.Level = LevelDebug
		}
		if f.DebugRTSP {
			cfg.EnableCategory(DebugRTSP)
			cfg.Level = LevelDebug
		}
		if f.DebugWebRTC {
			cfg.EnableCategory(DebugWebRTC)
			cfg.Level = LevelDebug
		}
	}

	return cfg, nil
}

// PrintUsageExamples prints usage examples for logging flags
func PrintUsageExamples() {
	examples := `
Logging Examples:

  Basic usage (INFO level, text format to stdout):
    ./relay

  Enable DEBUG level:
    ./relay --log-level debug
    ./relay -l debug

  Log to file:
    ./relay --log-file relay.log
    ./relay -o relay.log

  JSON format for structured logging:
    ./relay --log-format json -o relay.json

  Debug RTP packets only:
    ./relay --debug-rtp

  Debug NAL units only:
    ./relay --debug-nal

  Debug multiple categories:
    ./relay --debug-rtp --debug-nal --debug-track

  Debug everything:
    ./relay --debug-all -o debug.log

  Production logging (WARN level, JSON to file):
    ./relay -l warn --log-format json -o production.log
`
	fmt.Println(examples)
}

// String returns a string representation of enabled flags
func (f *Flags) String() string {
	var parts []string

	parts = append(parts, fmt.Sprintf("level=%s", f.LogLevel))
	parts = append(parts, fmt.Sprintf("format=%s", f.LogFormat))

	if f.LogFile != "" {
		parts = append(parts, fmt.Sprintf("output=%s", f.LogFile))
	} else {
		parts = append(parts, "output=stdout")
	}

	var debugCategories []string
	if f.DebugAll {
		debugCategories = append(debugCategories, "all")
	} else {
		if f.DebugRTP {
			debugCategories = append(debugCategories, "rtp")
		}
		if f.DebugNAL {
			debugCategories = append(debugCategories, "nal")
		}
		if f.DebugTrack {
			debugCategories = append(debugCategories, "track")
		}
		if f.DebugRTSP {
			debugCategories = append(debugCategories, "rtsp")
		}
		if f.DebugWebRTC {
			debugCategories = append(debugCategories, "webrtc")
		}
	}

	if len(debugCategories) > 0 {
		parts = append(parts, fmt.Sprintf("debug=[%s]", strings.Join(debugCategories, ",")))
	}

	return strings.Join(parts, " ")
}
