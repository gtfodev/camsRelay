# Bug Fix: 404 Errors in Multi-Camera Example

## Issue
The multi-camera example (`examples/multi_camera_example.go`) was getting 404 errors when attempting to generate RTSP streams, while the single-camera relay (`cmd/relay/main.go`) worked correctly.

## Root Cause
The `extractCameraDeviceID()` function in `pkg/nest/multi_manager.go` had a faulty heuristic for determining whether a camera ID was a full path or an already-extracted device ID:

```go
func extractCameraDeviceID(cameraID string) string {
    // WRONG: Nest device IDs are ~86 characters long
    if len(cameraID) < 30 {
        return cameraID
    }
    // This would try to extract from already-extracted IDs
    return extractDeviceID(cameraID)
}
```

### What Went Wrong
1. **Device IDs are 86 characters**: Nest device IDs like `AVPHwEtYJ6eztR1d7sSETV5BsnYWz3hdoMQAUOJjydZjayoQXdcmffuK0DAyjXFv2wQcEWgCSaaoc-3DzgvFvdmWuvUMuA` are 86 characters long
2. **Length check failed**: The check `len(cameraID) < 30` always returned false for real device IDs
3. **Wrong extraction attempt**: The code tried to extract from an already-extracted ID using `extractDeviceID()`
4. **Empty device ID**: Since the ID didn't contain `/devices/`, `extractDeviceID()` returned an empty string
5. **Malformed API call**: The API URL became `.../devices/:executeCommand` instead of `.../devices/{deviceID}:executeCommand`
6. **404 error**: Google's API returned 404 for the malformed URL

### Error Message
```
The requested URL /v1/enterprises/735ef91b-89b7-4f32-b8b9-5d54479be0bf/devices/:executeCommand was not found on this server.
```

Notice the missing device ID between `/devices/` and `:executeCommand`.

## Solution
Changed the heuristic to check for the presence of `/devices/` in the string, which correctly identifies full paths:

```go
func extractCameraDeviceID(cameraID string) string {
    // If it's a full path (contains "/devices/"), extract the device ID
    if contains(cameraID, "/devices/") {
        return extractDeviceID(cameraID)
    }
    // Otherwise, it's already just the device ID
    return cameraID
}
```

### Why This Works
- **Full path**: `enterprises/PROJECT/devices/DEVICEID` → Contains `/devices/` → Extract device ID
- **Already extracted**: `AVPHwEtYJ6eztR1d...` → No `/devices/` → Return as-is

## Testing

### Before Fix
```json
{
  "error": "generate RTSP stream: generate stream failed: ... 404 ... The requested URL /v1/enterprises/.../devices/:executeCommand was not found"
}
```

### After Fix
```json
{
  "msg": "generated RTSP stream",
  "device_id": "AVPHwEtYJ6eztR1d7sSETV5BsnYWz3hdoMQAUOJjydZjayoQXdcmffuK0DAyjXFv2wQcEWgCSaaoc-3DzgvFvdmWuvUMuA",
  "expires_at": "2025-12-16T17:13:50Z",
  "success": true,
  "error": null
}
```

### Unit Test Coverage
Created `pkg/nest/multi_manager_test.go` with test cases covering:
- Full path format with `/devices/`
- Short device IDs (< 30 chars)
- Real 86-character Nest device IDs
- Multiple device ID formats

All tests pass:
```
=== RUN   TestExtractCameraDeviceID
--- PASS: TestExtractCameraDeviceID (0.00s)
PASS
```

## Files Changed
- `pkg/nest/multi_manager.go`: Fixed `extractCameraDeviceID()` function (lines 535-542)
- `pkg/nest/multi_manager_test.go`: Added unit tests for device ID extraction

## Verification
Both examples now work correctly:
- ✅ Single-camera relay (`cmd/relay/main.go`) continues to work
- ✅ Multi-camera example (`examples/multi_camera_example.go`) now successfully generates streams with 12-second stagger interval
- ✅ Proper rate limiting at 10 QPM
- ✅ Automatic stream extension working

## Impact
This bug would have affected any code path using the `MultiStreamManager` that passed already-extracted device IDs (which is the standard pattern from `ListDevices()`). The single-camera relay worked because it bypasses `MultiStreamManager` and calls the client directly.
