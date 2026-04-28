//go:build windows

package lorca

import (
	"log"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	fxwUser32   = syscall.NewLazyDLL("user32.dll")
	fxwKernel32 = syscall.NewLazyDLL("kernel32.dll")
	fxwShell32  = syscall.NewLazyDLL("shell32.dll")
	fxwAdvapi32 = syscall.NewLazyDLL("advapi32.dll")

	fxwEnumWindows                 = fxwUser32.NewProc("EnumWindows")
	fxwGetWindowThreadProcessId    = fxwUser32.NewProc("GetWindowThreadProcessId")
	fxwSendMessageW                = fxwUser32.NewProc("SendMessageW")
	fxwIsWindowVisible             = fxwUser32.NewProc("IsWindowVisible")
	fxwLoadImageW                  = fxwUser32.NewProc("LoadImageW")
	fxwSetWindowTextW              = fxwUser32.NewProc("SetWindowTextW")
	fxwGetModuleHandleW            = fxwKernel32.NewProc("GetModuleHandleW")
	fxwCreateToolhelp32Snapshot    = fxwKernel32.NewProc("CreateToolhelp32Snapshot")
	fxwProcess32FirstW             = fxwKernel32.NewProc("Process32FirstW")
	fxwProcess32NextW              = fxwKernel32.NewProc("Process32NextW")
	fxwCloseHandle                 = fxwKernel32.NewProc("CloseHandle")
	fxwSHGetPropertyStoreForWindow = fxwShell32.NewProc("SHGetPropertyStoreForWindow")

	fxwRegCreateKeyExW = fxwAdvapi32.NewProc("RegCreateKeyExW")
	fxwRegSetValueExW  = fxwAdvapi32.NewProc("RegSetValueExW")
	fxwRegCloseKey     = fxwAdvapi32.NewProc("RegCloseKey")
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

// fxwIPropertyStore is the layout of an IPropertyStore COM object as seen from
// Go. The first field is a pointer to the vtable (an array of function
// pointers). Declaring the type this way lets us access the vtable without a
// uintptr->unsafe.Pointer conversion, which go vet flags as a potential misuse.
type fxwIPropertyStore struct{ vtbl *[8]uintptr }

// fxwProcessEntry32W mirrors the Windows PROCESSENTRY32W struct.
// Padding before th32DefaultHeapID matches the C layout on both 32-bit
// and 64-bit targets because Go aligns uintptr to its native size.
type fxwProcessEntry32W struct {
	dwSize              uint32
	cntUsage            uint32
	th32ProcessID       uint32
	th32DefaultHeapID   uintptr // ULONG_PTR - 4 bytes on 32-bit, 8 on 64-bit
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

// killFirefoxProcessTree kills all descendant processes of launcherPID,
// including orphaned children whose parent stub has already exited.
func killFirefoxProcessTree(launcherPID int, state *os.ProcessState) {
	pids := fxwDescendantPIDs(launcherPID)
	for childPID := range pids {
		if int(childPID) == launcherPID {
			continue
		}
		_ = killProcessTree(int(childPID))
	}
	if state == nil || !state.Exited() {
		_ = killProcessTree(launcherPID)
	}
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

// registerAUMIDDisplayName writes the AUMID and a human-readable display name
// into HKCU\Software\Classes\AppUserModelId so that Windows uses it as the
// taskbar group label instead of the host executable's file description.
// The display name is derived from the AUMID by taking the portion before the
// first '.', so "Company.App" becomes "Company".
func registerAUMIDDisplayName(aumid string) {
	displayName := aumid
	if dot := strings.IndexByte(aumid, '.'); dot >= 0 {
		displayName = aumid[:dot]
	}

	const hkcu uintptr = 0x80000001 // HKEY_CURRENT_USER
	const keyWrite = 0x20006        // KEY_WRITE
	const regSZ = 1                 // REG_SZ

	keyPath, err := syscall.UTF16PtrFromString(`Software\Classes\AppUserModelId\` + aumid)
	if err != nil {
		return
	}
	var hKey uintptr
	ret, _, _ := fxwRegCreateKeyExW.Call(
		hkcu, uintptr(unsafe.Pointer(keyPath)),
		0, 0, 0, keyWrite, 0,
		uintptr(unsafe.Pointer(&hKey)), 0,
	)
	if ret != 0 {
		log.Printf("lorca/firefox: registerAUMIDDisplayName: RegCreateKeyEx %08x", ret)
		return
	}
	defer fxwRegCloseKey.Call(hKey)

	valName, err := syscall.UTF16PtrFromString("DisplayName")
	if err != nil {
		return
	}
	valData := syscall.StringToUTF16(displayName)
	fxwRegSetValueExW.Call(
		hKey,
		uintptr(unsafe.Pointer(valName)),
		0, regSZ,
		uintptr(unsafe.Pointer(&valData[0])),
		uintptr(len(valData)*2),
	)
	runtime.KeepAlive(valData)
	runtime.KeepAlive(valName)
	runtime.KeepAlive(keyPath)
}

// applyFirefoxWindowAUMID sets the Windows App User Model ID on all visible
// top-level Firefox windows owned by the launcher's process tree.  Setting a
// unique AUMID causes Windows to group the window separately from regular
// Firefox instances in the taskbar and to use the window's own icon for the
// group button.
func applyFirefoxWindowAUMID(launcherPID int, aumid string) {
	// Register display name before Firefox windows appear so the taskbar picks
	// it up from the registry rather than using Firefox's file description.
	registerAUMIDDisplayName(aumid)

	// IID_IPropertyStore {886D8EEB-8CF2-4446-8D02-CDBA1DBDCF99}
	iid := [16]byte{
		0xEB, 0x8E, 0x6D, 0x88,
		0xF2, 0x8C,
		0x46, 0x44,
		0x8D, 0x02, 0xCD, 0xBA, 0x1D, 0xBD, 0xCF, 0x99,
	}
	// All PKEY_AppUserModel_* properties share the same FMTID:
	//   {9F4C2855-9F79-4B39-A8D0-E1D42DE1D5F3}
	// PKEY_AppUserModel_ID, pid=5
	pkey := [20]byte{
		0x55, 0x28, 0x4C, 0x9F, // fmtid.Data1 (little-endian)
		0x79, 0x9F, // fmtid.Data2 (little-endian)
		0x39, 0x4B, // fmtid.Data3 (little-endian)
		0xA8, 0xD0, 0xE1, 0xD4, 0x2D, 0xE1, 0xD5, 0xF3, // fmtid.Data4
		0x05, 0x00, 0x00, 0x00, // pid = 5
	}
	// PKEY_AppUserModel_PreventPinning, pid=9.
	// Setting this to VT_BOOL(TRUE) suppresses both the "Pin to taskbar" option
	// and the app relaunch action (e.g. "Firefox") from the taskbar context menu.
	pkeyPreventPin := [20]byte{
		0x55, 0x28, 0x4C, 0x9F,
		0x79, 0x9F,
		0x39, 0x4B,
		0xA8, 0xD0, 0xE1, 0xD4, 0x2D, 0xE1, 0xD5, 0xF3,
		0x09, 0x00, 0x00, 0x00, // pid = 9
	}
	aumidUTF16 := syscall.StringToUTF16(aumid)

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

		// pStore is declared as *fxwIPropertyStore so that the vtable can be
		// accessed without a uintptr->unsafe.Pointer conversion (which go vet
		// flags). SHGetPropertyStoreForWindow writes the COM pointer into the
		// variable via uintptr(unsafe.Pointer(&pStore)), which is the standard
		// syscall whitelisted pattern.
		var pStore *fxwIPropertyStore
		hr, _, _ := fxwSHGetPropertyStoreForWindow.Call(
			hwnd,
			uintptr(unsafe.Pointer(&iid[0])),
			uintptr(unsafe.Pointer(&pStore)),
		)
		if hr != 0 || pStore == nil {
			return 1
		}

		// Build VT_LPWSTR PROPVARIANT (24 bytes on 64-bit Windows):
		//   offset 0:  vt      = VT_LPWSTR (0x001F)
		//   offset 2-7: reserved (zero)
		//   offset 8:  pwszVal = pointer to UTF-16 string
		var pv [24]byte
		pv[0] = 0x1F // VT_LPWSTR low byte
		// Store pointer to aumidUTF16 data inside pv; aumidUTF16 is kept alive
		// by the closure capture below, so the GC will not collect it.
		*(*uintptr)(unsafe.Pointer(&pv[8])) = uintptr(unsafe.Pointer(&aumidUTF16[0]))

		// Build VT_BOOL PROPVARIANT for VARIANT_TRUE (0xFFFF):
		//   offset 0:  vt   = VT_BOOL (0x000B)
		//   offset 2-7: reserved (zero)
		//   offset 8-9: VARIANT_BOOL = 0xFFFF (TRUE)
		var pvBool [24]byte
		pvBool[0] = 0x0B // VT_BOOL low byte
		pvBool[8] = 0xFF // VARIANT_TRUE low byte
		pvBool[9] = 0xFF // VARIANT_TRUE high byte

		// IPropertyStore vtable: [0]QueryInterface [1]AddRef [2]Release
		//                        [3]GetCount [4]GetAt [5]GetValue [6]SetValue [7]Commit
		syscall.SyscallN(pStore.vtbl[6], // SetValue: PKEY_AppUserModel_ID
			uintptr(unsafe.Pointer(pStore)),
			uintptr(unsafe.Pointer(&pkey[0])),
			uintptr(unsafe.Pointer(&pv[0])),
		)
		syscall.SyscallN(pStore.vtbl[6], // SetValue: PKEY_AppUserModel_PreventPinning
			uintptr(unsafe.Pointer(pStore)),
			uintptr(unsafe.Pointer(&pkeyPreventPin[0])),
			uintptr(unsafe.Pointer(&pvBool[0])),
		)
		syscall.SyscallN(pStore.vtbl[7], uintptr(unsafe.Pointer(pStore))) // IPropertyStore::Commit
		syscall.SyscallN(pStore.vtbl[2], uintptr(unsafe.Pointer(pStore))) // IUnknown::Release

		// Explicit keep-alive: aumidUTF16 must not be GC'd while pv holds a raw
		// pointer into its backing array.
		runtime.KeepAlive(aumidUTF16)
		count++
		return 1
	})

	// Retry every 500ms until at least one visible Firefox window is found
	// (up to 10 attempts = 5s). Firefox windows may not be visible immediately.
	const maxAttempts = 10
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		time.Sleep(500 * time.Millisecond)
		count = 0
		fxwEnumWindows.Call(cb, 0)
		if count > 0 {
			log.Printf("lorca/firefox: applyWindowAUMID: set AUMID on %d Firefox window(s) (attempt %d)", count, attempt)
			runtime.KeepAlive(aumidUTF16)
			return
		}
	}
	log.Printf("lorca/firefox: applyWindowAUMID: no Firefox windows found after %d attempts", maxAttempts)
	runtime.KeepAlive(aumidUTF16)
}

// applyFirefoxWindowTitle sets the Win32 window title on all visible top-level
// Firefox windows owned by the launcher's process tree.  Firefox extends its
// client area over the non-client area via DWM composition; the text that shows
// through in the "title bar" region is the Win32 WM_CAPTION title, not a DOM
// element, so it cannot be changed via CSS.  SetWindowTextW overrides it.
// This must be called after the page has loaded so it fires after Firefox's own
// SetWindowText call that includes the " - Mozilla Firefox" suffix.
func applyFirefoxWindowTitle(launcherPID int, title string) {
	pids := fxwDescendantPIDs(launcherPID)
	if len(pids) == 0 {
		return
	}
	titlePtr, err := syscall.UTF16PtrFromString(title)
	if err != nil {
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
		fxwSetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(titlePtr)))
		count++
		return 1
	})
	fxwEnumWindows.Call(cb, 0)
	runtime.KeepAlive(titlePtr)
	log.Printf("lorca/firefox: applyWindowTitle: set title on %d Firefox window(s)", count)
}
