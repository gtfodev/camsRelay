/**
 * Viewer manages the WebRTC viewer lifecycle:
 * - Fetches camera list from backend
 * - Creates WebRTC connections to Cloudflare for each camera
 * - Handles reconnection on failures
 */

export class Viewer {
    constructor(grid) {
        this.grid = grid;
        this.cameras = new Map(); // cameraId -> CameraConnection
        this.config = null;
        this.refreshInterval = null;
    }

    async start() {
        console.log('[Viewer] Starting viewer');

        // Fetch Cloudflare configuration
        this.config = await this.fetchConfig();
        console.log('[Viewer] Loaded config:', this.config);

        // Initial camera fetch
        await this.refreshCameras();

        // Periodically refresh camera list (in case new cameras are added)
        this.refreshInterval = setInterval(() => {
            this.refreshCameras();
        }, 30000); // Every 30 seconds

        this.updateStatus('Connected', 'connected');
    }

    async stop() {
        if (this.refreshInterval) {
            clearInterval(this.refreshInterval);
        }

        // Close all camera connections
        for (const connection of this.cameras.values()) {
            connection.close();
        }
        this.cameras.clear();
    }

    async fetchConfig() {
        const response = await fetch('/api/config');
        if (!response.ok) {
            throw new Error(`Failed to fetch config: ${response.statusText}`);
        }
        return response.json();
    }

    async refreshCameras() {
        try {
            const response = await fetch('/api/cameras');
            if (!response.ok) {
                throw new Error(`Failed to fetch cameras: ${response.statusText}`);
            }

            const cameras = await response.json();
            console.log('[Viewer] Fetched cameras:', cameras);

            // Group cameras by cameraId (video + audio tracks)
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

            // Create connections for new cameras
            for (const [cameraId, cameraData] of cameraMap) {
                if (!this.cameras.has(cameraId)) {
                    console.log('[Viewer] Creating connection for camera:', cameraId);
                    const connection = new CameraConnection(
                        cameraData,
                        this.config.appId,
                        this.grid
                    );
                    this.cameras.set(cameraId, connection);
                    await connection.connect();
                }
            }

            // Remove cameras that are no longer available
            for (const [cameraId, connection] of this.cameras) {
                if (!cameraMap.has(cameraId)) {
                    console.log('[Viewer] Removing camera:', cameraId);
                    connection.close();
                    this.cameras.delete(cameraId);
                }
            }

            this.updateCameraCount(this.cameras.size);

        } catch (error) {
            console.error('[Viewer] Error refreshing cameras:', error);
            this.updateStatus('Error: ' + error.message, 'error');
        }
    }

    updateStatus(text, className) {
        const statusEl = document.getElementById('status');
        statusEl.textContent = text;
        statusEl.className = className;
    }

    updateCameraCount(count) {
        const countEl = document.getElementById('camera-count');
        countEl.textContent = `Cameras: ${count}`;
    }
}

/**
 * CameraConnection manages WebRTC connection for a single camera
 */
class CameraConnection {
    constructor(cameraData, appId, grid) {
        this.cameraData = cameraData;
        this.appId = appId;
        this.grid = grid;
        this.pc = null;
        this.tile = null;
        this.sessionId = null;
        this.reconnectAttempts = 0;
        this.maxReconnectAttempts = 5;
        this.reconnectDelay = 3000;
    }

    async connect() {
        console.log(`[Camera ${this.cameraData.id}] Connecting`);

        try {
            // Create tile in grid
            this.tile = this.grid.addCamera(this.cameraData.id, this.cameraData.name);
            this.updateStatus('connecting');

            // Create viewer session in Cloudflare
            this.sessionId = await this.createViewerSession();
            console.log(`[Camera ${this.cameraData.id}] Created viewer session:`, this.sessionId);

            // Pull remote tracks from producer session
            const offer = await this.pullRemoteTracks();
            console.log(`[Camera ${this.cameraData.id}] Received offer from Cloudflare`);

            // Create peer connection
            this.pc = new RTCPeerConnection({
                iceServers: [{ urls: 'stun:stun.l.google.com:19302' }]
            });

            // Handle incoming tracks
            this.pc.ontrack = (event) => {
                console.log(`[Camera ${this.cameraData.id}] Received track:`, event.track.kind);
                if (event.track.kind === 'video') {
                    this.tile.attachStream(event.streams[0]);
                }
            };

            // Handle connection state changes
            this.pc.onconnectionstatechange = () => {
                console.log(`[Camera ${this.cameraData.id}] Connection state:`, this.pc.connectionState);
                this.updateStatus(this.pc.connectionState);

                if (this.pc.connectionState === 'failed' || this.pc.connectionState === 'disconnected') {
                    this.handleDisconnect();
                } else if (this.pc.connectionState === 'connected') {
                    this.reconnectAttempts = 0; // Reset on successful connection
                }
            };

            // Handle ICE connection state changes
            this.pc.oniceconnectionstatechange = () => {
                console.log(`[Camera ${this.cameraData.id}] ICE state:`, this.pc.iceConnectionState);
            };

            // Set remote description (offer from Cloudflare)
            await this.pc.setRemoteDescription({
                type: 'offer',
                sdp: offer
            });

            // Create answer
            const answer = await this.pc.createAnswer();
            await this.pc.setLocalDescription(answer);

            // Send answer to Cloudflare
            await this.renegotiate(answer.sdp);

            console.log(`[Camera ${this.cameraData.id}] WebRTC negotiation complete`);

        } catch (error) {
            console.error(`[Camera ${this.cameraData.id}] Connection error:`, error);
            this.updateStatus('failed');
            this.tile.setError(error.message);
            this.scheduleReconnect();
        }
    }

    async createViewerSession() {
        const url = `https://rtc.live.cloudflare.com/v1/apps/${this.appId}/sessions/new`;
        const response = await fetch(url, {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json'
            }
        });

        if (!response.ok) {
            const body = await response.text();
            throw new Error(`Failed to create session: ${response.statusText} - ${body}`);
        }

        const data = await response.json();
        if (data.errorCode) {
            throw new Error(`Cloudflare error: ${data.errorCode} - ${data.errorDescription}`);
        }

        return data.sessionId;
    }

    async pullRemoteTracks() {
        const url = `https://rtc.live.cloudflare.com/v1/apps/${this.appId}/sessions/${this.sessionId}/tracks/new`;

        // Pull video and audio tracks from producer session
        const tracks = this.cameraData.tracks.map(track => ({
            location: 'remote',
            sessionId: this.cameraData.sessionId,
            trackName: track.trackName
        }));

        const response = await fetch(url, {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json'
            },
            body: JSON.stringify({ tracks })
        });

        if (!response.ok) {
            const body = await response.text();
            throw new Error(`Failed to pull tracks: ${response.statusText} - ${body}`);
        }

        const data = await response.json();
        if (data.errorCode) {
            throw new Error(`Cloudflare error: ${data.errorCode} - ${data.errorDescription}`);
        }

        if (!data.sessionDescription || !data.sessionDescription.sdp) {
            throw new Error('No SDP offer received from Cloudflare');
        }

        return data.sessionDescription.sdp;
    }

    async renegotiate(answerSdp) {
        const url = `https://rtc.live.cloudflare.com/v1/apps/${this.appId}/sessions/${this.sessionId}/renegotiate`;

        const response = await fetch(url, {
            method: 'PUT',
            headers: {
                'Content-Type': 'application/json'
            },
            body: JSON.stringify({
                sessionDescription: {
                    type: 'answer',
                    sdp: answerSdp
                }
            })
        });

        if (!response.ok) {
            const body = await response.text();
            throw new Error(`Failed to renegotiate: ${response.statusText} - ${body}`);
        }

        const data = await response.json();
        if (data.errorCode) {
            throw new Error(`Cloudflare error: ${data.errorCode} - ${data.errorDescription}`);
        }
    }

    handleDisconnect() {
        console.log(`[Camera ${this.cameraData.id}] Disconnected, attempting reconnect`);
        this.updateStatus('disconnected');
        this.scheduleReconnect();
    }

    scheduleReconnect() {
        if (this.reconnectAttempts >= this.maxReconnectAttempts) {
            console.error(`[Camera ${this.cameraData.id}] Max reconnect attempts reached`);
            this.updateStatus('failed');
            this.tile.setError('Connection failed after multiple attempts');
            return;
        }

        this.reconnectAttempts++;
        const delay = this.reconnectDelay * this.reconnectAttempts;

        console.log(`[Camera ${this.cameraData.id}] Reconnecting in ${delay}ms (attempt ${this.reconnectAttempts})`);

        setTimeout(() => {
            this.close();
            this.connect();
        }, delay);
    }

    updateStatus(status) {
        if (this.tile) {
            this.tile.setStatus(status);
        }
    }

    close() {
        if (this.pc) {
            this.pc.close();
            this.pc = null;
        }

        if (this.tile) {
            this.grid.removeCamera(this.cameraData.id);
            this.tile = null;
        }
    }
}
