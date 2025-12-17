/**
 * Viewer manages a SINGLE WebRTC session for ALL cameras
 * Optimized architecture:
 * - First camera: ~3s (full setup)
 * - Additional cameras: ~500ms (batch track pull)
 * - Camera switch: ~100ms (track update, no renegotiation)
 */

export class Viewer {
    constructor(grid) {
        this.grid = grid;
        this.cameras = new Map(); // cameraId -> CameraTile (UI only)
        this.config = null;
        this.refreshInterval = null;

        // Single session for all cameras
        this.sessionId = null;
        this.pc = null;
        this.trackMids = new Map(); // cameraId -> mid (for track updates)
        this.pendingTracks = new Map(); // trackName -> cameraId (to route ontrack events)
    }

    async start() {
        console.log('[Viewer] Starting viewer');

        this.config = await this.fetchConfig();

        // Create single viewer session
        await this.initSession();

        // Initial camera fetch
        await this.refreshCameras();

        this.refreshInterval = setInterval(() => {
            this.refreshCameras();
        }, 30000);

        this.updateStatus('Connected', 'connected');
    }

    async stop() {
        if (this.refreshInterval) {
            clearInterval(this.refreshInterval);
        }

        // Close all camera tiles
        for (const tile of this.cameras.values()) {
            tile.destroy();
        }
        this.cameras.clear();

        // Close peer connection
        if (this.pc) {
            this.pc.close();
            this.pc = null;
        }
    }

    async fetchConfig() {
        const response = await fetch('/api/config');
        if (!response.ok) {
            throw new Error(`Failed to fetch config: ${response.statusText}`);
        }
        return response.json();
    }

    async initSession() {
        // Create ONE session
        const response = await fetch('/api/cf/sessions/new', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' }
        });

        if (!response.ok) {
            throw new Error(`Failed to create session: ${response.statusText}`);
        }

        const data = await response.json();
        this.sessionId = data.sessionId;
        console.log('[Viewer] Created single viewer session:', this.sessionId);

        // Create ONE PeerConnection
        this.pc = new RTCPeerConnection({
            iceServers: [{ urls: 'stun:stun.l.google.com:19302' }]
        });

        this.pc.ontrack = (event) => {
            // Route track to correct camera tile
            const mid = event.transceiver?.mid;
            console.log('[Viewer] Received track:', event.track.kind, 'mid:', mid);

            // Find which camera this track belongs to
            for (const [cameraId, cameraMid] of this.trackMids) {
                if (cameraMid === mid && event.track.kind === 'video') {
                    const tile = this.cameras.get(cameraId);
                    if (tile) {
                        tile.attachStream(event.streams[0]);
                    }
                    break;
                }
            }
        };

        this.pc.onconnectionstatechange = () => {
            console.log('[Viewer] Connection state:', this.pc.connectionState);
            if (this.pc.connectionState === 'connected') {
                this.startStatsMonitoring();
            } else if (this.pc.connectionState === 'failed') {
                this.handleDisconnect();
            }
        };

        this.pc.oniceconnectionstatechange = () => {
            console.log('[Viewer] ICE state:', this.pc.iceConnectionState);
        };
    }

    async refreshCameras() {
        try {
            const response = await fetch('/api/cameras');
            if (!response.ok) {
                throw new Error(`Failed to fetch cameras: ${response.statusText}`);
            }

            const cameras = await response.json();

            // Group by cameraId
            const cameraMap = new Map();
            for (const camera of cameras) {
                if (!cameraMap.has(camera.cameraId)) {
                    cameraMap.set(camera.cameraId, {
                        id: camera.cameraId,
                        name: camera.name,
                        sessionId: camera.sessionId,
                        tracks: []
                    });
                }
                cameraMap.get(camera.cameraId).tracks.push({
                    trackName: camera.trackName,
                    kind: camera.kind
                });
            }

            // Find new cameras to add
            const newCameras = [];
            for (const [cameraId, cameraData] of cameraMap) {
                if (!this.cameras.has(cameraId)) {
                    // Create tile immediately
                    const tile = new CameraTile(cameraId, cameraData.name, this.grid);
                    this.cameras.set(cameraId, tile);
                    newCameras.push(cameraData);
                }
            }

            // Pull all new camera tracks in ONE request
            if (newCameras.length > 0) {
                await this.pullCameraTracks(newCameras);
            }

            // Remove cameras no longer available
            for (const [cameraId, tile] of this.cameras) {
                if (!cameraMap.has(cameraId)) {
                    await this.removeCamera(cameraId);
                }
            }

            this.updateCameraCount(this.cameras.size);

        } catch (error) {
            console.error('[Viewer] Error refreshing cameras:', error);
            this.updateStatus('Error: ' + error.message, 'error');
        }
    }

    async pullCameraTracks(camerasData) {
        // Build tracks array for ALL cameras at once
        const tracks = [];

        for (const camera of camerasData) {
            for (const track of camera.tracks) {
                tracks.push({
                    location: 'remote',
                    sessionId: camera.sessionId,
                    trackName: track.trackName
                });
                // Map trackName to cameraId for ontrack routing
                this.pendingTracks.set(track.trackName, camera.id);
            }
        }

        console.log('[Viewer] Pulling tracks for', camerasData.length, 'cameras:', tracks);

        const response = await fetch(`/api/cf/sessions/${this.sessionId}/tracks/new`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ tracks })
        });

        if (!response.ok) {
            throw new Error(`Failed to pull tracks: ${response.statusText}`);
        }

        const data = await response.json();

        // Map mids to cameras for ontrack routing
        if (data.tracks) {
            for (const track of data.tracks) {
                const cameraId = this.pendingTracks.get(track.trackName);
                if (cameraId && track.mid) {
                    this.trackMids.set(cameraId, track.mid);
                    console.log('[Viewer] Mapped camera', cameraId, 'to mid', track.mid);
                }
            }
        }

        // Handle SDP negotiation
        if (data.requiresImmediateRenegotiation && data.sessionDescription) {
            await this.handleNegotiation(data.sessionDescription);
        }
    }

    async handleNegotiation(offer) {
        console.log('[Viewer] Setting remote description');
        await this.pc.setRemoteDescription({
            type: offer.type,
            sdp: offer.sdp
        });

        const answer = await this.pc.createAnswer();
        await this.pc.setLocalDescription(answer);

        // Send answer
        await fetch(`/api/cf/sessions/${this.sessionId}/renegotiate`, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
                sessionDescription: { type: 'answer', sdp: answer.sdp }
            })
        });

        console.log('[Viewer] Renegotiation complete');
    }

    async removeCamera(cameraId) {
        const mid = this.trackMids.get(cameraId);

        if (mid) {
            // Close track in Cloudflare
            await fetch(`/api/cf/sessions/${this.sessionId}/tracks/close`, {
                method: 'PUT',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    tracks: [{ mid }],
                    force: true  // Don't renegotiate, just stop data
                })
            });
            this.trackMids.delete(cameraId);
        }

        const tile = this.cameras.get(cameraId);
        if (tile) {
            tile.destroy();
            this.cameras.delete(cameraId);
        }
    }

    // Fast camera switch - reuses existing transceiver
    async switchCamera(oldCameraId, newCameraData) {
        const mid = this.trackMids.get(oldCameraId);
        if (!mid) {
            // No existing transceiver, do full add
            await this.pullCameraTracks([newCameraData]);
            return;
        }

        console.log('[Viewer] Switching camera', oldCameraId, '->', newCameraData.id, 'on mid', mid);

        const response = await fetch(`/api/cf/sessions/${this.sessionId}/tracks/update`, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
                tracks: [{
                    location: 'remote',
                    sessionId: newCameraData.sessionId,
                    trackName: newCameraData.tracks[0].trackName,
                    mid: mid
                }]
            })
        });

        const data = await response.json();

        // Update our mappings
        this.trackMids.delete(oldCameraId);
        this.trackMids.set(newCameraData.id, mid);

        // Update tile
        const tile = this.cameras.get(oldCameraId);
        if (tile) {
            tile.updateCamera(newCameraData.id, newCameraData.name);
            this.cameras.delete(oldCameraId);
            this.cameras.set(newCameraData.id, tile);
        }

        // Usually no renegotiation needed for track update
        if (data.requiresImmediateRenegotiation) {
            await this.handleNegotiation(data.sessionDescription);
        }
    }

    handleDisconnect() {
        console.log('[Viewer] Disconnected');
        this.updateStatus('Disconnected', 'disconnected');
    }

    startStatsMonitoring() {
        // Monitor stats for the single PeerConnection
        this.statsInterval = setInterval(() => this.logStats(), 2000);
    }

    async logStats() {
        if (!this.pc) return;

        try {
            const stats = await this.pc.getStats();
            stats.forEach(report => {
                if (report.type === 'inbound-rtp' && report.kind === 'video') {
                    // Find which camera this corresponds to
                    const mid = report.mid;
                    for (const [cameraId, cameraMid] of this.trackMids) {
                        if (cameraMid === mid) {
                            const tile = this.cameras.get(cameraId);
                            if (tile) {
                                tile.updateStats({
                                    framesDecoded: report.framesDecoded || 0,
                                    framesReceived: report.framesReceived || 0,
                                    packetsReceived: report.packetsReceived || 0,
                                    packetsLost: report.packetsLost || 0
                                });
                            }
                            break;
                        }
                    }
                }
            });
        } catch (error) {
            console.error('[Viewer] Error getting stats:', error);
        }
    }

    updateStatus(text, className) {
        const statusEl = document.getElementById('status');
        if (statusEl) {
            statusEl.textContent = text;
            statusEl.className = className;
        }
    }

    updateCameraCount(count) {
        const countEl = document.getElementById('camera-count');
        if (countEl) {
            countEl.textContent = `Cameras: ${count}`;
        }
    }
}

/**
 * CameraTile - UI only, no WebRTC logic
 */
class CameraTile {
    constructor(cameraId, name, grid) {
        this.cameraId = cameraId;
        this.name = name;
        this.grid = grid;
        this.tile = grid.addCamera(cameraId, name);
    }

    attachStream(stream) {
        this.tile.attachStream(stream);
    }

    updateCamera(newId, newName) {
        this.cameraId = newId;
        this.name = newName;
        this.tile.setName(newName);
    }

    setStatus(status) {
        this.tile.setStatus(status);
    }

    updateStats(stats) {
        this.tile.updateStats(stats);
    }

    destroy() {
        this.grid.removeCamera(this.cameraId);
    }
}
