/**
 * Grid manages the visual layout of camera tiles
 */

export class Grid {
    constructor(container) {
        this.container = container;
        this.tiles = new Map(); // cameraId -> CameraTile
    }

    addCamera(cameraId, name) {
        if (this.tiles.has(cameraId)) {
            return this.tiles.get(cameraId);
        }

        const tile = new CameraTile(cameraId, name);
        this.tiles.set(cameraId, tile);
        this.container.appendChild(tile.element);

        return tile;
    }

    removeCamera(cameraId) {
        const tile = this.tiles.get(cameraId);
        if (tile) {
            tile.destroy();
            this.tiles.delete(cameraId);
        }
    }

    clear() {
        for (const tile of this.tiles.values()) {
            tile.destroy();
        }
        this.tiles.clear();
    }
}

/**
 * CameraTile represents a single camera in the grid
 */
class CameraTile {
    constructor(cameraId, name) {
        this.cameraId = cameraId;
        this.name = name;
        this.element = this.createTileElement();
        this.videoElement = this.element.querySelector('video');
        this.statusElement = this.element.querySelector('.camera-status');
        this.errorElement = this.element.querySelector('.error-message');
    }

    createTileElement() {
        const tile = document.createElement('div');
        tile.className = 'camera-tile';
        tile.innerHTML = `
            <div class="camera-header">
                <div class="camera-name">${this.escapeHtml(this.name)}</div>
                <div class="camera-status">Initializing</div>
            </div>
            <div class="video-container">
                <video autoplay playsinline muted></video>
                <div class="video-placeholder">
                    <div class="spinner"></div>
                </div>
            </div>
            <div class="camera-info">
                <div class="info-row">
                    <span>Camera ID:</span>
                    <span>${this.escapeHtml(this.cameraId)}</span>
                </div>
            </div>
            <div class="error-message" style="display: none;"></div>
        `;
        return tile;
    }

    attachStream(stream) {
        console.log(`[Tile ${this.cameraId}] Attaching stream`);

        // Diagnostic: Check stream state
        console.log(`[Tile ${this.cameraId}] Stream properties:`, {
            id: stream.id,
            active: stream.active,
            trackCount: stream.getTracks().length
        });

        // Diagnostic: Log each track
        const tracks = stream.getTracks();
        tracks.forEach((track, idx) => {
            console.log(`[Tile ${this.cameraId}] Track ${idx}:`, {
                kind: track.kind,
                id: track.id,
                label: track.label,
                enabled: track.enabled,
                muted: track.muted,
                readyState: track.readyState
            });
        });

        this.videoElement.srcObject = stream;

        // Add all video element event handlers for diagnostics
        this.videoElement.onloadstart = () => {
            console.log(`[Tile ${this.cameraId}] Video loadstart event`);
        };

        this.videoElement.onloadedmetadata = () => {
            console.log(`[Tile ${this.cameraId}] Video metadata loaded`, {
                videoWidth: this.videoElement.videoWidth,
                videoHeight: this.videoElement.videoHeight,
                readyState: this.videoElement.readyState,
                networkState: this.videoElement.networkState
            });
            const placeholder = this.element.querySelector('.video-placeholder');
            if (placeholder) {
                placeholder.style.display = 'none';
            }
        };

        this.videoElement.onloadeddata = () => {
            console.log(`[Tile ${this.cameraId}] Video data loaded`);
        };

        this.videoElement.oncanplay = () => {
            console.log(`[Tile ${this.cameraId}] Video can play`);
        };

        this.videoElement.oncanplaythrough = () => {
            console.log(`[Tile ${this.cameraId}] Video can play through`);
        };

        this.videoElement.onplaying = () => {
            console.log(`[Tile ${this.cameraId}] Video playing`);
        };

        this.videoElement.onstalled = () => {
            console.warn(`[Tile ${this.cameraId}] Video stalled`);
        };

        this.videoElement.onsuspend = () => {
            console.warn(`[Tile ${this.cameraId}] Video suspended`);
        };

        this.videoElement.onwaiting = () => {
            console.warn(`[Tile ${this.cameraId}] Video waiting`);
        };

        this.videoElement.onerror = (error) => {
            console.error(`[Tile ${this.cameraId}] Video error:`, error);
            console.error(`[Tile ${this.cameraId}] Video element state:`, {
                error: this.videoElement.error,
                readyState: this.videoElement.readyState,
                networkState: this.videoElement.networkState
            });
            this.setError('Video playback error');
        };

        // Explicitly try to play
        this.videoElement.play().then(() => {
            console.log(`[Tile ${this.cameraId}] Play promise resolved`);
        }).catch((err) => {
            console.error(`[Tile ${this.cameraId}] Play promise rejected:`, err);
        });

        // Monitor track state changes
        tracks.forEach((track, idx) => {
            track.onended = () => {
                console.warn(`[Tile ${this.cameraId}] Track ${idx} ended`);
            };
            track.onmute = () => {
                console.warn(`[Tile ${this.cameraId}] Track ${idx} muted`);
            };
            track.onunmute = () => {
                console.log(`[Tile ${this.cameraId}] Track ${idx} unmuted`);
            };
        });
    }

    setStatus(status) {
        this.statusElement.textContent = status;
        this.statusElement.className = `camera-status ${status}`;
        this.element.className = `camera-tile ${status}`;

        // Hide error on successful connection
        if (status === 'connected') {
            this.clearError();
        }
    }

    setError(message) {
        this.errorElement.textContent = message;
        this.errorElement.style.display = 'block';
    }

    clearError() {
        this.errorElement.style.display = 'none';
    }

    destroy() {
        // Stop video stream
        if (this.videoElement.srcObject) {
            const stream = this.videoElement.srcObject;
            stream.getTracks().forEach(track => track.stop());
            this.videoElement.srcObject = null;
        }

        // Remove from DOM
        this.element.remove();
    }

    escapeHtml(text) {
        const div = document.createElement('div');
        div.textContent = text;
        return div.innerHTML;
    }
}
