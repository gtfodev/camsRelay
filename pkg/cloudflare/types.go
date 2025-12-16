package cloudflare

// SessionDescription represents an SDP offer or answer
type SessionDescription struct {
	SDP  string `json:"sdp"`
	Type string `json:"type"` // "offer" or "answer"
}

// TrackObject represents a media track
type TrackObject struct {
	Location  string  `json:"location"` // "local" or "remote"
	Mid       string  `json:"mid,omitempty"`
	SessionID string  `json:"sessionId,omitempty"`
	TrackName string  `json:"trackName"`
	Kind      string  `json:"kind,omitempty"` // "audio" or "video"
	ErrorCode string  `json:"errorCode,omitempty"`
	ErrorDesc string  `json:"errorDescription,omitempty"`
	Status    string  `json:"status,omitempty"` // "active", "inactive", "waiting"
}

// NewSessionResponse is returned when creating a new session
type NewSessionResponse struct {
	SessionID          string              `json:"sessionId"`
	SessionDescription *SessionDescription `json:"sessionDescription,omitempty"`
	ErrorCode          string              `json:"errorCode,omitempty"`
	ErrorDesc          string              `json:"errorDescription,omitempty"`
}

// TracksRequest is used to add tracks to a session
type TracksRequest struct {
	SessionDescription *SessionDescription `json:"sessionDescription,omitempty"`
	Tracks             []TrackObject       `json:"tracks"`
	AutoDiscover       bool                `json:"autoDiscover,omitempty"`
}

// TracksResponse is returned when adding tracks
type TracksResponse struct {
	RequiresImmediateRenegotiation bool                `json:"requiresImmediateRenegotiation"`
	SessionDescription             *SessionDescription `json:"sessionDescription,omitempty"`
	Tracks                         []TrackObject       `json:"tracks"`
	ErrorCode                      string              `json:"errorCode,omitempty"`
	ErrorDesc                      string              `json:"errorDescription,omitempty"`
}

// RenegotiateRequest is used to renegotiate a session
type RenegotiateRequest struct {
	SessionDescription SessionDescription `json:"sessionDescription"`
}

// RenegotiateResponse is returned after renegotiation
type RenegotiateResponse struct {
	SessionDescription *SessionDescription `json:"sessionDescription,omitempty"`
	ErrorCode          string              `json:"errorCode,omitempty"`
	ErrorDesc          string              `json:"errorDescription,omitempty"`
}

// CloseTrackObject identifies a track to close
type CloseTrackObject struct {
	Mid string `json:"mid"`
}

// CloseTracksRequest is used to close tracks
type CloseTracksRequest struct {
	SessionDescription *SessionDescription `json:"sessionDescription,omitempty"`
	Tracks             []CloseTrackObject  `json:"tracks"`
	Force              bool                `json:"force,omitempty"`
}

// CloseTracksResponse is returned when closing tracks
type CloseTracksResponse struct {
	SessionDescription             *SessionDescription `json:"sessionDescription,omitempty"`
	Tracks                         []CloseTrackObject  `json:"tracks"`
	RequiresImmediateRenegotiation bool                `json:"requiresImmediateRenegotiation"`
	ErrorCode                      string              `json:"errorCode,omitempty"`
	ErrorDesc                      string              `json:"errorDescription,omitempty"`
}

// GetSessionStateResponse returns the current session state
type GetSessionStateResponse struct {
	Tracks    []TrackObject `json:"tracks"`
	ErrorCode string        `json:"errorCode,omitempty"`
	ErrorDesc string        `json:"errorDescription,omitempty"`
}
