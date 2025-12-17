package api

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethan/nest-cloudflare-relay/pkg/cloudflare"
	"github.com/ethan/nest-cloudflare-relay/pkg/relay"
)

//go:embed web/*
var webFS embed.FS

// Server provides HTTP API for camera session discovery and web viewer
type Server struct {
	relay       *relay.MultiCameraRelay
	cfClient    *cloudflare.Client
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
	cfClient *cloudflare.Client,
	appID string,
	logger *slog.Logger,
) *Server {
	return &Server{
		relay:       relay,
		cfClient:    cfClient,
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
	mux.HandleFunc("/api/debug/session", s.handleDebugSession)

	// Cloudflare proxy endpoints (authenticated on backend)
	mux.HandleFunc("/api/cf/sessions/new", s.handleCreateSession)
	mux.HandleFunc("/api/cf/sessions/", s.handleSessionOperation)

	// Static file server for viewer using embedded filesystem
	staticFS, err := fs.Sub(webFS, "web/static")
	if err != nil {
		return err
	}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Index page handler
	mux.HandleFunc("/", s.handleIndex)

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
		// Use a timeout channel to prevent blocking indefinitely
		type statsResult struct {
			stats []relay.RelayStats
			err   error
		}
		statsChan := make(chan statsResult, 1)

		// Fetch stats in goroutine with timeout protection
		go func() {
			defer func() {
				if r := recover(); r != nil {
					s.logger.Error("panic in GetRelayStats", "panic", r)
					statsChan <- statsResult{err: fmt.Errorf("panic: %v", r)}
				}
			}()
			stats := s.relay.GetRelayStats()
			statsChan <- statsResult{stats: stats}
		}()

		// Wait for stats with timeout
		var stats []relay.RelayStats
		select {
		case result := <-statsChan:
			if result.err != nil {
				s.logger.Error("failed to get relay stats", "error", result.err)
				// Return empty array on error
				stats = nil
			} else {
				stats = result.stats
			}
		case <-time.After(5 * time.Second):
			s.logger.Error("timeout getting relay stats")
			// Return empty array on timeout
			stats = nil
		}

		if stats != nil {
			s.mu.RLock()
			cameras = make([]CameraInfo, 0, len(stats)) // Only video tracks
			for _, stat := range stats {
				name := s.cameraNames[stat.CameraID]
				if name == "" {
					name = stat.CameraID
				}

				// Video track only (audio not currently populated)
				cameras = append(cameras, CameraInfo{
					CameraID:  stat.CameraID,
					SessionID: stat.SessionID,
					TrackName: "video",
					Name:      name,
					Kind:      "video",
				})
			}
			s.mu.RUnlock()
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(cameras); err != nil {
		s.logger.Error("failed to encode cameras response", "error", err)
	}
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

// handleIndex serves the main viewer page from embedded filesystem
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	// Read index.html from embedded filesystem
	indexHTML, err := webFS.ReadFile("web/index.html")
	if err != nil {
		s.logger.Error("failed to read index.html", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
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

// handleCreateSession proxies session creation requests to Cloudflare
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	// Create session via Cloudflare client (authenticated)
	resp, err := s.cfClient.CreateSession(ctx)
	if err != nil {
		s.logger.Error("failed to create session", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return response to frontend
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleSessionOperation routes session-specific operations
func (s *Server) handleSessionOperation(w http.ResponseWriter, r *http.Request) {
	// Parse session ID from URL: /api/cf/sessions/{sessionId}/...
	path := strings.TrimPrefix(r.URL.Path, "/api/cf/sessions/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		http.Error(w, "invalid session path", http.StatusBadRequest)
		return
	}

	sessionID := parts[0]
	operation := parts[1]

	switch operation {
	case "tracks":
		if len(parts) >= 3 {
			switch parts[2] {
			case "new":
				s.handleAddTracks(w, r, sessionID)
			case "update":
				s.handleUpdateTracks(w, r, sessionID)
			case "close":
				s.handleCloseTracks(w, r, sessionID)
			default:
				http.Error(w, "invalid tracks operation", http.StatusBadRequest)
			}
		} else {
			http.Error(w, "invalid tracks operation", http.StatusBadRequest)
		}
	case "renegotiate":
		s.handleRenegotiate(w, r, sessionID)
	default:
		http.Error(w, "unknown operation", http.StatusNotFound)
	}
}

// handleAddTracks proxies track addition requests to Cloudflare
func (s *Server) handleAddTracks(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	// Parse request body
	var req cloudflare.TracksRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.Error("failed to parse tracks request", "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Log the request for debugging
	s.logger.Info("viewer pulling tracks",
		"viewer_session_id", sessionID,
		"tracks", req.Tracks)

	// Add tracks via Cloudflare client (authenticated)
	resp, err := s.cfClient.AddTracks(ctx, sessionID, &req)
	if err != nil {
		s.logger.Error("failed to add tracks",
			"session_id", sessionID,
			"error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Log the response for debugging
	s.logger.Info("Cloudflare AddTracks response",
		"viewer_session_id", sessionID,
		"requires_renegotiation", resp.RequiresImmediateRenegotiation,
		"tracks", resp.Tracks)

	// Return response to frontend
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleDebugSession provides debug information about camera sessions
func (s *Server) handleDebugSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionIDParam := r.URL.Query().Get("sessionId")
	if sessionIDParam == "" {
		http.Error(w, "sessionId parameter required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Get session state from Cloudflare
	stateResp, err := s.cfClient.GetSessionState(ctx, sessionIDParam)
	if err != nil {
		s.logger.Error("failed to get session state",
			"session_id", sessionIDParam,
			"error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stateResp)
}

// handleUpdateTracks proxies track update requests to Cloudflare
func (s *Server) handleUpdateTracks(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	// Parse request body
	var req cloudflare.UpdateTracksRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.Error("failed to parse update tracks request", "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	s.logger.Info("viewer updating tracks",
		"viewer_session_id", sessionID,
		"tracks", req.Tracks)

	// Update tracks via Cloudflare client (authenticated)
	resp, err := s.cfClient.UpdateTracks(ctx, sessionID, &req)
	if err != nil {
		s.logger.Error("failed to update tracks",
			"session_id", sessionID,
			"error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.logger.Info("Cloudflare UpdateTracks response",
		"viewer_session_id", sessionID,
		"requires_renegotiation", resp.RequiresImmediateRenegotiation,
		"tracks", resp.Tracks)

	// Return response to frontend
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleCloseTracks proxies track close requests to Cloudflare
func (s *Server) handleCloseTracks(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	// Parse request body
	var req cloudflare.CloseTracksRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.Error("failed to parse close tracks request", "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	s.logger.Info("viewer closing tracks",
		"viewer_session_id", sessionID,
		"tracks", req.Tracks,
		"force", req.Force)

	// Close tracks via Cloudflare client (authenticated)
	resp, err := s.cfClient.CloseTracks(ctx, sessionID, &req)
	if err != nil {
		s.logger.Error("failed to close tracks",
			"session_id", sessionID,
			"error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.logger.Info("Cloudflare CloseTracks response",
		"viewer_session_id", sessionID,
		"requires_renegotiation", resp.RequiresImmediateRenegotiation,
		"tracks", resp.Tracks)

	// Return response to frontend
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleRenegotiate proxies renegotiation requests to Cloudflare
func (s *Server) handleRenegotiate(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	// Parse request body
	var req cloudflare.RenegotiateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.Error("failed to parse renegotiate request", "error", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Renegotiate via Cloudflare client (authenticated)
	resp, err := s.cfClient.Renegotiate(ctx, sessionID, &req)
	if err != nil {
		s.logger.Error("failed to renegotiate",
			"session_id", sessionID,
			"error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return response to frontend
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
