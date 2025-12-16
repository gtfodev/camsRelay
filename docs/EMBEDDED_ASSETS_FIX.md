# Embedded Assets Fix

## Problem

The web viewer didn't work when running the multi-relay binary from directories other than the project root.

### Root Cause

The API server in `pkg/api/server.go` used relative filesystem paths:
- `http.FileServer(http.Dir("web/static"))` - Line 69
- `http.ServeFile(w, r, "web/index.html")` - Line 180

These paths only worked when the current working directory (CWD) was `/home/ethan/cams/`. Running the binary from elsewhere caused 404 errors.

## Solution

Embedded the web assets directly into the binary using Go's `embed` package.

### Implementation

1. **Moved web directory**
   - From: `/home/ethan/cams/web/`
   - To: `/home/ethan/cams/pkg/api/web/`
   - Reason: `go:embed` requires files to be in the same package or subdirectories

2. **Added embed directive**
   ```go
   //go:embed web/*
   var webFS embed.FS
   ```

3. **Updated file serving**
   - Static files: Use `embed.FS` with `fs.Sub()` and `http.FS()`
   - Index page: Read from `webFS.ReadFile("web/index.html")`

### Code Changes

**pkg/api/server.go:**
```go
import (
    "embed"
    "io/fs"
    // ... other imports
)

//go:embed web/*
var webFS embed.FS

// In Start() method:
staticFS, err := fs.Sub(webFS, "web/static")
if err != nil {
    return err
}
mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

// In handleIndex():
indexHTML, err := webFS.ReadFile("web/index.html")
if err != nil {
    // handle error
}
w.Header().Set("Content-Type", "text/html; charset=utf-8")
w.Write(indexHTML)
```

## Benefits

1. **Path Independence**: Binary works from any directory
2. **Single Binary Deployment**: No external file dependencies
3. **Simpler Distribution**: Just copy the binary
4. **No Runtime Path Resolution**: Eliminates entire class of path bugs

## Trade-offs

- **Binary Size**: Increased from ~13MB to ~16MB (+3MB for web assets)
- **Update Process**: Changing web files requires rebuild (acceptable for this use case)

## Testing

### Verification Steps

1. Build binary:
   ```bash
   go build -o multi-relay ./cmd/multi-relay
   ```

2. Test from different directory:
   ```bash
   cd /tmp
   /home/ethan/cams/multi-relay  # (requires .env)
   curl http://localhost:8080/
   curl http://localhost:8080/static/js/viewer.js
   ```

3. Verify embedded files:
   ```bash
   ./scripts/verify_embedded_serving.sh
   ```

### Test Results

- Build successful: ✓
- Binary size: 16MB ✓
- Files embedded: 5 files (index.html, 2 JS, 1 CSS, 1 README) ✓
- Can run from any directory: ✓

## File Structure

```
pkg/api/
├── server.go          # Updated to use embed.FS
└── web/               # Embedded into binary
    ├── index.html
    ├── README.md
    └── static/
        ├── css/
        │   └── style.css
        └── js/
            ├── grid.js
            └── viewer.js
```

## Git Changes

```
M  .gitignore                             # Added multi-relay patterns
M  pkg/api/server.go                      # Use embed.FS
R  web/README.md -> pkg/api/web/README.md # Moved for embed
R  web/index.html -> pkg/api/web/index.html
R  web/static/css/style.css -> pkg/api/web/static/css/style.css
R  web/static/js/grid.js -> pkg/api/web/static/js/grid.js
R  web/static/js/viewer.js -> pkg/api/web/static/js/viewer.js
```

## Documentation Updates

- `pkg/api/web/README.md`: Added note about `go:embed` implementation
- `.gitignore`: Added `/multi-relay` and `/cmd/multi-relay/multi-relay` patterns

## Future Considerations

If web assets change frequently during development:
- Consider build flag to toggle between embedded and filesystem serving
- Example:
  ```go
  //go:build !dev
  //go:embed web/*
  var webFS embed.FS
  ```

For production deployments, embedded assets are the correct solution.

## Commit

```
commit 7d14b79
fix: embed web assets into binary for reliable serving

- Use go:embed to include web/ directory in binary
- Eliminates path resolution issues when running from any directory
- Moved web/ to pkg/api/web/ for embed compatibility
```
