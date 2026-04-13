//go:build windows

package lorca

import (
	"log"
	"syscall"
	"time"
	"unsafe"
)

var (
	fxwUser32   = syscall.NewLazyDLL("user32.dll")
	fxwKernel32 = syscall.NewLazyDLL("kernel32.dll")

	fxwEnumWindows              = fxwUser32.NewProc("EnumWindows")
	fxwGetWindowThreadProcessId = fxwUser32.NewProc("GetWindowThreadProcessId")
	fxwSendMessageW             = fxwUser32.NewProc("SendMessageW")
	fxwIsWindowVisible          = fxwUser32.NewProc("IsWindowVisible")
	fxwLoadImageW               = fxwUser32.NewProc("LoadImageW")
	fxwGetModuleHandleW         = fxwKernel32.NewProc("GetModuleHandleW")
	fxwCreateToolhelp32Snapshot = fxwKernel32.NewProc("CreateToolhelp32Snapshot")
	fxwProcess32FirstW          = fxwKernel32.NewProc("Process32FirstW")
	fxwProcess32NextW           = fxwKernel32.NewProc("Process32NextW")
	fxwCloseHandle              = fxwKernel32.NewProc("CloseHandle")
)

const (
	fxwWMSetIcon         = uintptr(0x0080)
	fxwIconSmall         = uintptr(0)
	fxwIconBig           = uintptr(1)
	fxwImageIcon         = uintptr(1)
	fxwLRDefaultSize     = uintptr(0x0040)
	fxwLRLoadFromFile    = uintptr(0x0010)
	fxwLRShared          = uintptr(0x8000)
	fxwTH32CSSnapProcess = uintptr(0x00000002)
	fxwInvalidHandle     = ^uintptr(0)
)

// fxwProcessEntry32W mirrors the Windows PROCESSENTRY32W struct.
// Padding before th32DefaultHeapID matches the C layout on both 32-bit
// and 64-bit targets because Go aligns uintptr to its native size.
type fxwProcessEntry32W struct {
	dwSize              uint32
	cntUsage            uint32
	th32ProcessID       uint32
	th32DefaultHeapID   uintptr  // ULONG_PTR - 4 bytes on 32-bit, 8 on 64-bit
	th32ModuleID        uint32
	cntThreads          uint32
	th32ParentProcessID uint32
	pcPriClassBase      int32
	dwFlags             uint32
	szExeFile           [260]uint16
}

// fxwDescendantPIDs returns the set containing launcherPID and all of its
// descendant process IDs at the time of the snapshot.  On Windows, Firefox
// launches a stub that exits immediately; the real browser processes keep
// the stub's PID as their th32ParentProcessID even after the stub exits.
func fxwDescendantPIDs(launcherPID int) map[uint32]bool {
	snap, _, _ := fxwCreateToolhelp32Snapshot.Call(fxwTH32CSSnapProcess, 0)
	if snap == fxwInvalidHandle {
		return nil
	}
	defer fxwCloseHandle.Call(snap)

	children := make(map[uint32][]uint32)
	var e fxwProcessEntry32W
	e.dwSize = uint32(unsafe.Sizeof(e))
	ret, _, _ := fxwProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(&e)))
	for ret != 0 {
		children[e.th32ParentProcessID] = append(children[e.th32ParentProcessID], e.th32ProcessID)
		e.dwSize = uint32(unsafe.Sizeof(e))
		ret, _, _ = fxwProcess32NextW.Call(snap, uintptr(unsafe.Pointer(&e)))
	}

	result := map[uint32]bool{uint32(launcherPID): true}
	queue := []uint32{uint32(launcherPID)}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, child := range children[cur] {
			if !result[child] {
				result[child] = true
				queue = append(queue, child)
			}
		}
	}
	return result
}

// applyFirefoxWindowIcon sets WM_SETICON on all top-level visible windows
// owned by the Firefox process tree.  iconPath, if non-empty, is the path to
// a .ico file (including PNG-in-ICO) that is loaded with LR_LOADFROMFILE.
// When iconPath is empty, the function falls back to PE icon resource 1 from
// the host executable (the goversioninfo/rsrc convention).
//
// Called from a background goroutine so the 500ms delay doesn't block startup.
func applyFirefoxWindowIcon(launcherPID int, iconPath string) {
	// Wait for Firefox to create and show its main window.
	time.Sleep(500 * time.Millisecond)

	var hIcon uintptr
	if iconPath != "" {
		// Load the icon from the supplied file path.
		// hInstance must be NULL when LR_LOADFROMFILE is set.
		pathPtr, err := syscall.UTF16PtrFromString(iconPath)
		if err == nil {
			hIcon, _, _ = fxwLoadImageW.Call(0, uintptr(unsafe.Pointer(pathPtr)), fxwImageIcon, 0, 0, fxwLRDefaultSize|fxwLRLoadFromFile)
		}
	}
	if hIcon == 0 {
		// Fallback: load icon resource 1 from the host executable.
		hInst, _, _ := fxwGetModuleHandleW.Call(0)
		hIcon, _, _ = fxwLoadImageW.Call(hInst, 1, fxwImageIcon, 0, 0, fxwLRDefaultSize|fxwLRShared)
	}
	if hIcon == 0 {
		log.Printf("lorca/firefox: applyWindowIcon: could not load icon (path=%q)", iconPath)
		return
	}

	pids := fxwDescendantPIDs(launcherPID)
	if len(pids) == 0 {
		return
	}

	count := 0
	cb := syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		var pid uint32
		fxwGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if !pids[pid] {
			return 1
		}
		visible, _, _ := fxwIsWindowVisible.Call(hwnd)
		if visible == 0 {
			return 1
		}
		fxwSendMessageW.Call(hwnd, fxwWMSetIcon, fxwIconSmall, hIcon)
		fxwSendMessageW.Call(hwnd, fxwWMSetIcon, fxwIconBig, hIcon)
		count++
		return 1
	})
	fxwEnumWindows.Call(cb, 0)
	log.Printf("lorca/firefox: applyWindowIcon: set icon on %d Firefox window(s)", count)
}
