//go:build !windows

package lorca

import "os"

// killFirefoxProcessTree kills the Firefox launcher process on non-Windows
// platforms. On Windows, the real implementation (in firefox_windows.go) also
// kills orphaned child processes left behind by the launcher stub.
func killFirefoxProcessTree(pid int, state *os.ProcessState) {
	if state == nil || !state.Exited() {
		killProcessTree(pid)
	}
}

// applyFirefoxWindowIcon is a no-op on non-Windows platforms.
// On Windows, it uses WM_SETICON to apply an icon to the Firefox window
// (see firefox_windows.go). iconPath is the path to a .ico file; if empty,
// the host executable's PE resource 1 is used as a fallback.
func applyFirefoxWindowIcon(_ int, _ string) {
	// Not implemented: changing a launched process's window icon requires
	// platform-specific APIs (Win32 WM_SETICON on Windows, Cocoa on macOS,
	// _NET_WM_ICON via X11 on Linux) and CGO is not permitted in this module.
	// The Windows implementation lives in firefox_windows.go.
}

// applyFirefoxWindowTitle is a no-op on non-Windows platforms.
// On Windows, it calls SetWindowTextW to override the Win32 window caption.
func applyFirefoxWindowTitle(_ int, _ string) {
	// Win32 SetWindowTextW is Windows-only; no equivalent cross-platform API
	// is available without CGO. See firefox_windows.go.
}

// applyFirefoxWindowAUMID is a no-op on non-Windows platforms.
// On Windows, it uses SHGetPropertyStoreForWindow to set the AUMID on the
// Firefox window so it groups separately in the taskbar (see firefox_windows.go).
func applyFirefoxWindowAUMID(_ int, _ string) {
	// IPropertyStore/SHGetPropertyStoreForWindow is Windows-only. See firefox_windows.go.
}
