package lorca

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// ChromeExecutable returns a string which points to the preferred Chrome
// executable file.
var ChromeExecutable = LocateChrome

var _foundPaths = []string{}

func FoundPaths() []string {
	if len(_foundPaths) == 0 {
		LocateChrome("")
	}
	return _foundPaths
}

func RefreshFoundPaths() {
	_foundPaths = []string{}
	LocateChrome("")
}

// LocateChrome returns a path to the Chrome binary, or an empty string if
// Chrome installation is not found.
func LocateChrome(preferPath string) string {

	// If preferPath is specified and it exists
	if preferPath != "" {
		if _, err := os.Stat(preferPath); err == nil {
			return preferPath
		}
	}

	// If env variable "LORCACHROME" specified and it exists
	if path, ok := os.LookupEnv("LORCACHROME"); ok {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	var paths []string
	switch runtime.GOOS {
	case "darwin":
		paths = []string{
			//Chrome
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/google-chrome",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			//Brave
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			//Opera
			"/Applications/Opera.app/Contents/MacOS/Opera",
			//Vivaldi
			"/Applications/Vivaldi.app/Contents/MacOS/Vivaldi",
			//Edge (why)
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		}
	case "windows":
		paths = []string{
			//Chrome
			os.Getenv("LocalAppData") + "/Google/Chrome/Application/chrome.exe",
			os.Getenv("ProgramFiles") + "/Google/Chrome/Application/chrome.exe",
			os.Getenv("ProgramFiles(x86)") + "/Google/Chrome/Application/chrome.exe",
			os.Getenv("LocalAppData") + "/Chromium/Application/chrome.exe",
			os.Getenv("ProgramFiles") + "/Chromium/Application/chrome.exe",
			os.Getenv("ProgramFiles(x86)") + "/Chromium/Application/chrome.exe",
			//Opera
			os.Getenv("LocalAppData") + "/Programs/Opera/launcher.exe",
			os.Getenv("LocalAppData") + "/Programs/Opera/opera.exe",
			os.Getenv("ProgramFiles") + "/Opera/launcher.exe",
			os.Getenv("ProgramFiles") + "/Opera/opera.exe",
			//Brave
			os.Getenv("LocalAppData") + "/BraveSoftware/Brave-Browser/Application/brave.exe",
			os.Getenv("ProgramFiles") + "/BraveSoftware/Brave-Browser/Application/brave.exe",
			//Vivaldi
			os.Getenv("LocalAppData") + "/Vivaldi/Application/vivaldi.exe",
			os.Getenv("ProgramFiles") + "/Vivaldi/Application/vivaldi.exe",
			//Edge
			os.Getenv("ProgramFiles(x86)") + "/Microsoft/Edge/Application/msedge.exe",
			os.Getenv("ProgramFiles") + "/Microsoft/Edge/Application/msedge.exe",
		}
	default:
		paths = []string{
			//Chrome
			"/usr/bin/google-chrome-stable",
			"/usr/bin/google-chrome",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/snap/bin/chromium",
			//Opera
			"/usr/bin/opera",
			//Brave
			"/usr/bin/brave-browser",
			//Vivaldi
			"/usr/bin/vivaldi",
		}
	}

	foundPath := ""
	for _, path := range paths {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		if foundPath == "" {
			foundPath = path
			_foundPaths = append(_foundPaths, path)
		} else {
			_foundPaths = append(_foundPaths, path)
		}
	}

	return foundPath
}

// PromptDownload asks user if he wants to download and install Chrome, and
// opens a download web page if the user agrees.
func PromptDownload() {
	title := "Chrome not found"
	text := "No Chrome/Chromium installation was found. Would you like to download and install it now?"

	// Ask user for confirmation
	if !messageBox(title, text) {
		return
	}

	// Open download page
	url := "https://www.google.com/chrome/"
	switch runtime.GOOS {
	case "linux":
		exec.Command("xdg-open", url).Run()
	case "darwin":
		exec.Command("open", url).Run()
	case "windows":
		r := strings.NewReplacer("&", "^&")
		exec.Command("cmd", "/c", "start", r.Replace(url)).Run()
	}
}
