//go:build !windows

package lorca

// applyFirefoxWindowIcon is a no-op on non-Windows platforms.
// On Windows, it uses WM_SETICON to apply the host executable's icon
// to the Firefox window (see firefox_windows.go).
func applyFirefoxWindowIcon(_ int) {
	// Not implemented: changing a launched process's window icon requires
	// platform-specific APIs (Win32 WM_SETICON on Windows, Cocoa on macOS,
	// _NET_WM_ICON via X11 on Linux) and CGO is not permitted in this module.
	// The Windows implementation lives in firefox_windows.go.
}
