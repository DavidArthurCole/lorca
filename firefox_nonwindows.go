//go:build !windows

package lorca

// applyFirefoxWindowIcon is a no-op on non-Windows platforms.
// On Windows, it uses WM_SETICON to apply the host executable's icon
// to the Firefox window (see firefox_windows.go).
func applyFirefoxWindowIcon(_ int) {}
