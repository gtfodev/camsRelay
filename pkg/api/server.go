package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ethan/nest-cloudflare-relay/pkg/relay"
)

// Server provides HTTP API for camera session discovery and web viewer
type Server struct {
	relay       *relay.MultiCameraRelay
	appID       string
	logger      *slog.Logger
	httpServer  *http.Server
	mu          sync.RWMutex
	cameraNames map[string]string // cameraID -> display name
}

// CameraInfo represents a camera's session information for the viewer
type CameraInfo struct {
	CameraID  string `json:"cameraId"`
	SessionID string `json:"sessionId"`
	TrackName string `json:"trackName"`
	Name      string `json:"name"`
	Kind      string `json:"kind"` // "video" or "audio"
}

// ConfigResponse provides Cloudflare configuration for the viewer
type ConfigResponse struct {
	AppID string `json:"appId"`
}

// NewServer creates a new API server
func NewServer(
	relay *relay.MultiCameraRelay,
	appID string,
	logger *slog.Logger,
) *Server {
	return &Server{
		relay:       relay,
		appID:       appID,
		logger:      logger,
		cameraNames: make(map[string]string),
	}
}

// SetCameraName sets a display name for a camera
func (s *Server) SetCameraName(cameraID, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cameraNames[cameraID] = name
}

// Start starts the HTTP server
func (s *Server) Start(ctx context.Context, addr string) error {
	mux := http.NewServeMux()

	// API endpoints
	mux.HandleFunc("/api/cameras", s.handleGetCameras)
	mux.HandleFunc("/api/config", s.handleGetConfig)

	// Static file server for viewer
	mux.HandleFunc("/", s.handleIndex)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))

	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: s.withCORS(s.withLogging(mux)),
		// Add timeouts to prevent resource exhaustion
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}

	s.logger.Info("starting HTTP server", "address", addr)

	// Start server in goroutine
	errChan := make(chan error, 1)
	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("HTTP server error", "error", err)
			errChan <- err
		}
	}()

	// Give the server a moment to start and check for immediate errors
	select {
	case err := <-errChan:
		return err
	case <-time.After(100 * time.Millisecond):
		// Server started successfully
		return nil
	}
}

// Stop gracefully stops the HTTP server
func (s *Server) Stop(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}

	s.logger.Info("stopping HTTP server")
	return s.httpServer.Shutdown(ctx)
}

// handleGetCameras returns list of active camera sessions
func (s *Server) handleGetCameras(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Handle case where relay is not initialized yet
	cameras := make([]CameraInfo, 0)

	if s.relay != nil {
		stats := s.relay.GetRelayStats()

		s.mu.RLock()
		cameras = make([]CameraInfo, 0, len(stats)*2) // *2 for video and audio tracks
		for _, stat := range stats {
			name := s.cameraNames[stat.CameraID]
			if name == "" {
				name = stat.CameraID
			}

			// Video track
			cameras = append(cameras, CameraInfo{
				CameraID:  stat.CameraID,
				SessionID: stat.SessionID,
				TrackName: "video",
				Name:      name,
				Kind:      "video",
			})

			// Audio track
			cameras = append(cameras, CameraInfo{
				CameraID:  stat.CameraID,
				SessionID: stat.SessionID,
				TrackName: "audio",
				Name:      name,
				Kind:      "audio",
			})
		}
		s.mu.RUnlock()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cameras)
}

// handleGetConfig returns Cloudflare configuration
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	response := ConfigResponse{
		AppID: s.appID,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleIndex serves the main viewer page
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	http.ServeFile(w, r, "web/index.html")
}

// withCORS adds CORS headers to responses
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// withLogging adds request logging
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrap response writer to capture status code
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		s.logger.Info("HTTP request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote_addr", r.RemoteAddr,
		)
	})
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}
