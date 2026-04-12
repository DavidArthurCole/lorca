package lorca

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
)

// UI interface allows talking to the HTML5 UI from Go.
type UI interface {
	Load(url string) error
	Bounds() (Bounds, error)
	SetBounds(Bounds) error
	Bind(name string, f interface{}) error
	Eval(js string) Value
	Done() <-chan struct{}
	Close() error
	GetDebugPort() int
}

type ui struct {
	browser browserImpl
	relay   *relay
	tmpDir  string
}

var defaultChromeArgs = []string{
	"--disable-background-networking",
	"--disable-background-timer-throttling",
	"--disable-backgrounding-occluded-windows",
	"--disable-breakpad",
	"--disable-crash-reporter",
	"--disable-client-side-phishing-detection",
	"--disable-default-apps",
	"--disable-dev-shm-usage",
	"--disable-infobars",
	"--disable-extensions",
	"--disable-features=site-per-process,BlockInsecurePrivateNetworkRequests,PrivateNetworkAccessChecks",
	"--disable-hang-monitor",
	"--disable-ipc-flooding-protection",
	"--disable-popup-blocking",
	"--disable-prompt-on-repost",
	"--disable-renderer-backgrounding",
	"--disable-sync",
	"--disable-translate",
	"--disable-windows10-custom-titlebar",
	"--metrics-recording-only",
	"--no-first-run",
	"--no-default-browser-check",
	"--safebrowsing-disable-auto-update",
	//"--enable-automation", https://github.com/zserge/lorca/issues/167
	"--password-store=basic",
	"--use-mock-keychain",
	"--remote-allow-origins=*",
	"--enable-logging --v=1",
}

// BrowserHint tells NewWithBrowser which browser backend to use.
type BrowserHint string

const (
	// BrowserAuto selects the backend by inspecting the resolved binary path.
	BrowserAuto BrowserHint = "auto"
	// BrowserChrome selects the Chromium-family CDP backend.
	BrowserChrome BrowserHint = "chrome"
	// BrowserFirefox selects the Firefox WebDriver BiDi backend.
	BrowserFirefox BrowserHint = "firefox"
)

// NewWithBrowser is like New but lets the caller specify which browser backend
// to use. hint == BrowserAuto inspects the resolved binary name; a path
// containing "firefox" (case-insensitive) selects the Firefox backend.
func NewWithBrowser(url, dir, preferPath string, width, height int, hint BrowserHint, customArgs ...string) (UI, error) {
	if url == "" {
		url = "data:text/html,<html></html>"
	}
	tmpDir := ""
	if dir == "" {
		name, err := os.MkdirTemp("", "lorca")
		if err != nil {
			return nil, err
		}
		dir, tmpDir = name, name
	}

	r, err := newRelay()
	if err != nil {
		return nil, err
	}

	// Resolve binary and select backend.
	var binary string
	useFirefox := false
	switch hint {
	case BrowserFirefox:
		binary = LocateFirefox(preferPath)
		if binary == "" {
			r.close()
			return nil, errors.New("lorca: no Firefox binary found")
		}
		useFirefox = true
	default: // BrowserChrome or BrowserAuto
		binary = ChromeExecutable(preferPath)
		if hint == BrowserAuto {
			useFirefox = strings.Contains(strings.ToLower(binary), "firefox")
		}
	}

	var browser browserImpl
	if useFirefox {
		args := append(append([]string{}, defaultFirefoxArgs...),
			"--profile", dir,
			fmt.Sprintf("--window-size=%d,%d", width, height),
		)
		args = append(args, customArgs...)
		args = append(args, url)
		browser, err = newFirefoxWithArgs(binary, args...)
	} else {
		args := append(append([]string{}, defaultChromeArgs...),
			fmt.Sprintf("--app=%s", url),
			fmt.Sprintf("--user-data-dir=%s", dir),
			fmt.Sprintf("--window-size=%d,%d", width, height),
		)
		args = append(args, customArgs...)
		browser, err = newChromeWithArgs(binary, args...)
	}
	if err != nil {
		r.close()
		return nil, err
	}

	if err := browser.injectScript(r.bootstrapScript()); err != nil {
		r.close()
		browser.kill()
		<-browser.done()
		return nil, err
	}

	return &ui{browser: browser, relay: r, tmpDir: tmpDir}, nil
}

// New returns a new HTML5 UI for the given URL, user profile directory, window
// size and other options passed to the browser engine. If URL is an empty
// string - a blank page is displayed. If user profile directory is an empty
// string - a temporary directory is created and it will be removed on
// ui.Close(). You might want to use "--headless" custom CLI argument to test
// your UI code.
func New(url, dir, preferPath string, width, height int, customArgs ...string) (UI, error) {
	return NewWithBrowser(url, dir, preferPath, width, height, BrowserAuto, customArgs...)
}

func (u *ui) Done() <-chan struct{} {
	return u.browser.done()
}

func (u *ui) Close() error {
	u.relay.close()
	// ignore err, as the browser process might be already dead, when user closes the window.
	u.browser.kill()
	<-u.browser.done()
	if u.tmpDir != "" {
		if err := os.RemoveAll(u.tmpDir); err != nil {
			return err
		}
	}
	return nil
}

func (u *ui) Load(url string) error { return u.browser.load(url) }

func (u *ui) Bind(name string, f interface{}) error {
	v := reflect.ValueOf(f)
	// f must be a function
	if v.Kind() != reflect.Func {
		return errors.New("only functions can be bound")
	}
	// f must return either value and error or just error
	if n := v.Type().NumOut(); n > 2 {
		return errors.New("function may only return a value or a value+error")
	}

	if err := u.relay.bind(name, func(raw []json.RawMessage) (interface{}, error) {
		if len(raw) != v.Type().NumIn() {
			return nil, errors.New("function arguments mismatch")
		}
		args := []reflect.Value{}
		for i := range raw {
			arg := reflect.New(v.Type().In(i))
			if err := json.Unmarshal(raw[i], arg.Interface()); err != nil {
				return nil, err
			}
			args = append(args, arg.Elem())
		}
		errorType := reflect.TypeOf((*error)(nil)).Elem()
		res := v.Call(args)
		switch len(res) {
		case 0:
			// No results from the function, just return nil
			return nil, nil
		case 1:
			// One result may be a value, or an error
			if res[0].Type().Implements(errorType) {
				if res[0].Interface() != nil {
					return nil, res[0].Interface().(error)
				}
				return nil, nil
			}
			return res[0].Interface(), nil
		case 2:
			// Two results: first one is value, second is error
			if !res[1].Type().Implements(errorType) {
				return nil, errors.New("second return value must be an error")
			}
			if res[1].Interface() == nil {
				return res[0].Interface(), nil
			}
			return res[0].Interface(), res[1].Interface().(error)
		default:
			return nil, errors.New("unexpected number of return values")
		}
	}); err != nil {
		return err
	}
	// Install the binding on the current page immediately AND register it for
	// all future page loads via addScriptToEvaluateOnNewDocument. This ensures
	// bound functions are available synchronously before any page JS runs,
	// avoiding the race where a page mounts before the relay WebSocket has
	// delivered its register messages.
	return u.browser.injectBinding(name)
}

func (u *ui) Eval(js string) Value {
	v, err := u.browser.eval(js)
	return value{err: err, raw: v}
}

func (u *ui) SetBounds(b Bounds) error {
	return u.browser.setBounds(b)
}

func (u *ui) Bounds() (Bounds, error) {
	return u.browser.bounds()
}

func (u *ui) GetDebugPort() int {
	switch b := u.browser.(type) {
	case *chrome:
		return b.debugPort
	case *firefox:
		return b.debugPort
	default:
		return 0
	}
}
