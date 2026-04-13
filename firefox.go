package lorca

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var defaultFirefoxArgs = []string{
	"--no-remote",
	"--new-instance",
}

// bidiValueToJSON converts a WebDriver BiDi serialized value to plain JSON.
// BiDi wraps every value in a {"type":"...", "value":...} envelope; this
// function unwraps it recursively so callers get standard json.RawMessage.
func bidiValueToJSON(v json.RawMessage) (json.RawMessage, error) {
	var wrapper struct {
		Type  string          `json:"type"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(v, &wrapper); err != nil {
		return nil, err
	}
	switch wrapper.Type {
	case "string", "number", "boolean":
		return wrapper.Value, nil
	case "null", "undefined":
		return json.RawMessage("null"), nil
	case "array":
		var items []json.RawMessage
		if err := json.Unmarshal(wrapper.Value, &items); err != nil {
			return nil, err
		}
		converted := make([]json.RawMessage, len(items))
		for i, item := range items {
			c, err := bidiValueToJSON(item)
			if err != nil {
				return nil, err
			}
			converted[i] = c
		}
		b, err := json.Marshal(converted)
		return json.RawMessage(b), err
	case "object":
		// BiDi object value is [[keyString, bidiValue], ...]
		var pairs [][2]json.RawMessage
		if err := json.Unmarshal(wrapper.Value, &pairs); err != nil {
			return nil, err
		}
		obj := make(map[string]json.RawMessage, len(pairs))
		for _, pair := range pairs {
			var key string
			if err := json.Unmarshal(pair[0], &key); err != nil {
				return nil, err
			}
			val, err := bidiValueToJSON(pair[1])
			if err != nil {
				return nil, err
			}
			obj[key] = val
		}
		b, err := json.Marshal(obj)
		return json.RawMessage(b), err
	default:
		return json.RawMessage("null"), nil
	}
}

// bidiConn is a minimal WebSocket client for the Firefox WebDriver BiDi
// protocol. golang.org/x/net/websocket always sends an Origin header which
// Firefox's /session endpoint rejects with 400 Bad Request, so we implement
// our own handshake and RFC 6455 text-frame framing.
type bidiConn struct {
	conn    net.Conn
	br      *bufio.Reader
	writeMu sync.Mutex
}

func newBidiConn(wsLoc *url.URL) (*bidiConn, error) {
	addr := wsLoc.Host
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, err
	}
	bc := &bidiConn{conn: conn, br: bufio.NewReader(conn)}
	if err := bc.handshake(wsLoc.Host, wsLoc.Path); err != nil {
		conn.Close()
		return nil, err
	}
	return bc, nil
}

func (c *bidiConn) handshake(host, path string) error {
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Protocol: webdriver-bidi\r\n" +
		"\r\n"

	c.conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.conn.Write([]byte(req)); err != nil {
		return err
	}

	// Read status line.
	statusLine, err := c.br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("firefox: BiDi handshake read: %w", err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 101") {
		return fmt.Errorf("firefox: BiDi handshake: %s", strings.TrimSpace(statusLine))
	}
	// Drain the rest of the HTTP headers.
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("firefox: BiDi handshake drain: %w", err)
		}
		if line == "\r\n" {
			break
		}
	}
	c.conn.SetDeadline(time.Time{})
	return nil
}

// writeFrame encodes a single WebSocket frame with the given opcode and
// masked payload and writes it to the connection. All client frames must
// be masked per RFC 6455 §5.3.
func (c *bidiConn) writeFrame(opcode byte, payload []byte) error {
	plen := len(payload)
	var header []byte
	header = append(header, 0x80|opcode) // FIN=1, RSV=0

	var maskKey [4]byte
	rand.Read(maskKey[:])

	switch {
	case plen <= 125:
		header = append(header, byte(0x80|plen))
	case plen <= 65535:
		header = append(header, 0xFE, byte(plen>>8), byte(plen))
	default:
		header = append(header, 0xFF,
			0, 0, 0, 0,
			byte(plen>>24), byte(plen>>16), byte(plen>>8), byte(plen))
	}
	header = append(header, maskKey[:]...)

	masked := make([]byte, plen)
	for i, b := range payload {
		masked[i] = b ^ maskKey[i%4]
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.conn.Write(append(header, masked...))
	return err
}

// Send marshals v to JSON and sends it as a WebSocket text frame.
func (c *bidiConn) Send(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.writeFrame(0x1, data)
}

// Receive reads the next WebSocket text/binary frame and unmarshals its
// payload into v. Control frames (ping/pong/close) are handled inline.
func (c *bidiConn) Receive(v interface{}) error {
	for {
		hdr := make([]byte, 2)
		if _, err := io.ReadFull(c.br, hdr); err != nil {
			return err
		}

		opcode := hdr[0] & 0x0F
		isMasked := hdr[1]&0x80 != 0
		plen := int(hdr[1] & 0x7F)

		switch plen {
		case 126:
			ext := make([]byte, 2)
			if _, err := io.ReadFull(c.br, ext); err != nil {
				return err
			}
			plen = int(ext[0])<<8 | int(ext[1])
		case 127:
			ext := make([]byte, 8)
			if _, err := io.ReadFull(c.br, ext); err != nil {
				return err
			}
			// Lower 32 bits suffice; payloads > 4 GB are not expected.
			plen = int(ext[4])<<24 | int(ext[5])<<16 | int(ext[6])<<8 | int(ext[7])
		}

		var maskKey [4]byte
		if isMasked {
			if _, err := io.ReadFull(c.br, maskKey[:]); err != nil {
				return err
			}
		}

		payload := make([]byte, plen)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return err
		}
		if isMasked {
			for i := range payload {
				payload[i] ^= maskKey[i%4]
			}
		}

		switch opcode {
		case 0x8: // close
			return io.EOF
		case 0x9: // ping -reply with pong
			c.writeFrame(0xA, payload)
			continue
		case 0xA: // pong -ignore
			continue
		case 0x0, 0x1, 0x2: // continuation, text, binary
			return json.Unmarshal(payload, v)
		default:
			continue // unknown opcode; skip
		}
	}
}

// Close closes the underlying TCP connection.
func (c *bidiConn) Close() error {
	return c.conn.Close()
}

type firefox struct {
	sync.Mutex
	cmd         *exec.Cmd
	bidi        *bidiConn
	id          int32
	context     string // WebDriver BiDi browsing context ID
	pending     map[int]chan result
	doneC       chan struct{}
	doneOnce    sync.Once
	lastBounds  Bounds
	debugPort   int
	loadScripts []string // scripts re-eval'd in page realm on every browsingContext.load
}

// closeDone closes doneC exactly once.  It is called by readLoop on exit so
// that callers blocking on Done() are unblocked when the BiDi connection drops
// (i.e. when the real Firefox process closes), not when the launcher stub exits.
func (f *firefox) closeDone() {
	f.doneOnce.Do(func() { close(f.doneC) })
}

func newFirefoxWithArgs(binary string, args ...string) (*firefox, error) {
	f := &firefox{
		id:      1, // 0 used for session.new, 1 for getTree during init; send() increments before use
		pending: map[int]chan result{},
	}

	debugPort, err := getFreePort()
	if err != nil {
		return nil, err
	}
	f.debugPort = debugPort

	args = append(args, fmt.Sprintf("--remote-debugging-port=%d", debugPort))
	f.cmd = exec.Command(binary, args...)
	if err := f.cmd.Start(); err != nil {
		return nil, err
	}

	// Poll /json/version until Firefox is ready (same retry loop as Chrome).
	startTime := time.Now()
	var res *http.Response
	for {
		res, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", debugPort))
		if err == nil {
			break
		}
		if time.Since(startTime) > 5*time.Second {
			killProcessTree(f.cmd.Process.Pid)
			return nil, fmt.Errorf("firefox: failed to reach /json/version within 5 seconds: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		killProcessTree(f.cmd.Process.Pid)
		return nil, err
	}

	// Firefox responds with HTML at /json/version rather than JSON.
	// Ignore parse errors and always use the /session BiDi endpoint.
	var ver browserVersion
	_ = json.Unmarshal(body, &ver)

	var wsLoc *url.URL
	if ver.WebSocketDebuggerUrl != "" {
		wsLoc, err = url.Parse(ver.WebSocketDebuggerUrl)
		if err != nil {
			killProcessTree(f.cmd.Process.Pid)
			return nil, err
		}
	} else {
		// Firefox's BiDi WebSocket lives at /session. The golang.org/x/net/websocket
		// library always sends an Origin header which Firefox rejects; we use our own
		// bidiConn that omits the Origin header entirely.
		wsLoc = &url.URL{
			Scheme: "ws",
			Host:   fmt.Sprintf("127.0.0.1:%d", debugPort),
			Path:   "/session",
		}
	}

	f.bidi, err = newBidiConn(wsLoc)
	if err != nil {
		killProcessTree(f.cmd.Process.Pid)
		return nil, err
	}

	// Activate WebDriver BiDi session.
	if err := f.bidi.Send(h{"id": 0, "method": "session.new", "params": h{"capabilities": h{}}}); err != nil {
		f.bidi.Close()
		killProcessTree(f.cmd.Process.Pid)
		return nil, err
	}
	for {
		var m struct {
			ID int `json:"id"`
		}
		if err := f.bidi.Receive(&m); err != nil {
			f.bidi.Close()
			killProcessTree(f.cmd.Process.Pid)
			return nil, fmt.Errorf("firefox: waiting for session.new response: %w", err)
		}
		if m.ID == 0 {
			break
		}
	}

	// Get the initial browsing context ID.
	if err := f.bidi.Send(h{"id": 1, "method": "browsingContext.getTree", "params": h{}}); err != nil {
		f.bidi.Close()
		killProcessTree(f.cmd.Process.Pid)
		return nil, err
	}
	for {
		var m struct {
			ID     int `json:"id"`
			Result struct {
				Contexts []struct {
					Context string `json:"context"`
				} `json:"contexts"`
			} `json:"result"`
		}
		if err := f.bidi.Receive(&m); err != nil {
			f.bidi.Close()
			killProcessTree(f.cmd.Process.Pid)
			return nil, fmt.Errorf("firefox: waiting for browsingContext.getTree response: %w", err)
		}
		if m.ID == 1 {
			if len(m.Result.Contexts) == 0 {
				f.bidi.Close()
				killProcessTree(f.cmd.Process.Pid)
				return nil, errors.New("firefox: no browsing contexts found")
			}
			f.context = m.Result.Contexts[0].Context
			break
		}
	}

	f.doneC = make(chan struct{})
	go func() {
		err := f.cmd.Wait()
		// On Windows, firefox.exe is a launcher stub: it starts the real Firefox
		// child process and exits immediately (exit 0).  cmd.Wait() therefore
		// returns at once, long before the UI is ready.  We do NOT close doneC
		// here; it is closed by readLoop when the BiDi connection drops, which
		// correctly reflects when the real Firefox process is gone.
		log.Printf("lorca/firefox: launcher process exited err=%v state=%v", err, f.cmd.ProcessState)
	}()
	go f.readLoop()

	// readLoop is now running; f.send can safely be used.
	// Subscribe to console logs, navigation events, and realm creation for diagnostics.
	if _, err := f.send("session.subscribe", h{"events": []string{
		"log.entryAdded",
		"browsingContext.navigationStarted",
		"browsingContext.load",
		"browsingContext.contextDestroyed",
		"script.realmCreated",
	}}); err != nil {
		log.Printf("lorca/firefox: session.subscribe failed: %v", err)
	}

	// Apply the host executable's icon to Firefox's window on platforms that
	// support it.  Runs in a background goroutine to avoid blocking startup.
	go applyFirefoxWindowIcon(f.cmd.Process.Pid)

	return f, nil
}

func (f *firefox) send(method string, params h) (json.RawMessage, error) {
	id := int(atomic.AddInt32(&f.id, 1))
	resc := make(chan result, 1)
	f.Lock()
	f.pending[id] = resc
	f.Unlock()

	err := f.bidi.Send(h{"id": id, "method": method, "params": params})
	if err != nil {
		f.Lock()
		delete(f.pending, id)
		f.Unlock()
		return nil, err
	}
	res := <-resc
	return res.Value, res.Err
}

// sendNoWait sends a BiDi command and immediately returns without waiting for
// a response.  The response, when it arrives, is silently discarded by
// readLoop (no entry in f.pending).  This is safe to call from within
// readLoop because bidiConn.Send uses a separate write mutex.
func (f *firefox) sendNoWait(method string, params h) {
	id := int(atomic.AddInt32(&f.id, 1))
	// Intentionally no f.pending entry -response is dropped.
	_ = f.bidi.Send(h{"id": id, "method": method, "params": params})
}

// bidiMsg is the shape of every message Firefox sends over the BiDi WebSocket.
// BiDi error responses have "error" as a string error-code (e.g. "unknown error")
// and a separate top-level "message" string -NOT a nested {"message":"..."} object.
type bidiMsg struct {
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   string          `json:"error"`   // BiDi error code string, e.g. "unknown error"
	Message string          `json:"message"` // BiDi error message string
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (f *firefox) readLoop() {
	// Signal doneC when the BiDi connection drops.  This is the authoritative
	// signal that Firefox is gone: on Windows the cmd.Wait() goroutine fires
	// immediately (launcher stub exits), so we cannot rely on it.
	defer f.closeDone()
	for {
		var m bidiMsg
		if err := f.bidi.Receive(&m); err != nil {
			log.Printf("lorca/firefox: readLoop exiting: %v", err)
			return
		}
		if m.Method != "" {
			switch m.Method {
			case "browsingContext.contextDestroyed":
				log.Printf("lorca/firefox contextDestroyed raw: %s", string(m.Params))
				params := struct {
					Context string `json:"context"`
				}{}
				json.Unmarshal(m.Params, &params)
				if params.Context == f.context {
					log.Printf("lorca/firefox: main context destroyed -killing")
					f.kill()
					return
				}
			case "log.entryAdded":
				log.Printf("lorca/firefox log.entryAdded raw: %s", string(m.Params))
			case "browsingContext.navigationStarted":
				log.Printf("lorca/firefox navigationStarted raw: %s", string(m.Params))
			case "browsingContext.load":
				log.Printf("lorca/firefox load raw: %s", string(m.Params))
				// Re-eval all scripts (bootstrap first, then bindings) in the page
				// window realm via a single script.evaluate call.  f.eval targets the
				// page window realm via target:{context}, creating page-realm objects.
				// Combining into one call avoids 50+ round-trips and is faster.
				//
				// A goroutine is used because f.eval calls f.send which blocks waiting
				// for readLoop to deliver the response - calling it inline would deadlock.
				var loadParams struct {
					Context string `json:"context"`
				}
				json.Unmarshal(m.Params, &loadParams)
				if loadParams.Context == f.context {
					f.Lock()
					scripts := append([]string(nil), f.loadScripts...)
					f.Unlock()
					if len(scripts) > 0 {
						go func(scripts []string) {
							combined := strings.Join(scripts, ";\n")
							log.Printf("lorca/firefox: post-load re-eval %d script(s) in page realm", len(scripts))
							if _, err := f.eval(combined); err != nil {
								log.Printf("lorca/firefox: post-load eval error: %v", err)
							} else {
								log.Printf("lorca/firefox: post-load re-eval complete")
							}
						}(scripts)
					}
				}
			case "script.realmCreated":
				log.Printf("lorca/firefox realmCreated raw: %s", string(m.Params))
				// When the real-origin window realm (origin != null) is created for
				// our context, immediately send all loadScripts (bootstrap first, then
				// bindings) as a single script.evaluate, without spawning a goroutine
				// or waiting for a response.
				//
				// loadScripts contains all scripts that must run in the page window realm
				// on every page load: the bootstrap (window.__lorcaWS etc.) followed by
				// the binding functions.  Since no preloads are registered, none of these
				// objects are sandbox-realm - they are only ever created by script.evaluate
				// with target:{context}, which always resolves to the page window realm.
				//
				// We send here (no goroutine) so the evaluate is queued in the BiDi
				// send buffer immediately.  The module bundle must still be fetched over
				// localhost before Vue runs, giving us a window to get ahead.
				//
				// Post-load re-eval (browsingContext.load handler) is the unconditional
				// fallback if this race is lost.
				//
				// NOTE: No script.addPreloadScript is registered anywhere in this code
				// path, so there are no preload sandbox realms to worry about.  The
				// Sandbox check below is a safety guard in case addPreloadScript is
				// added in the future.
				var realmParams struct {
					Realm   string `json:"realm"`
					Origin  string `json:"origin"`
					Context string `json:"context"`
					Type    string `json:"type"`
					Sandbox string `json:"sandbox"`
				}
				if err := json.Unmarshal(m.Params, &realmParams); err == nil &&
					realmParams.Sandbox == "" &&
					realmParams.Context == f.context &&
					realmParams.Type == "window" &&
					realmParams.Origin != "" && realmParams.Origin != "null" {
					f.Lock()
					scripts := append([]string(nil), f.loadScripts...)
					f.Unlock()
					if len(scripts) > 0 {
						combined := strings.Join(scripts, ";\n")
						log.Printf("lorca/firefox: realmCreated realm=%s firing %d script(s) early (no-wait)", realmParams.Realm, len(scripts))
						f.sendNoWait("script.evaluate", h{
							"expression":      combined,
							"awaitPromise":    false,
							"target":          h{"context": f.context},
							"resultOwnership": "none",
						})
					}
				}
			default:
				log.Printf("lorca/firefox: unhandled event %s: %s", m.Method, string(m.Params))
			}
			continue
		}
		// Response -route to pending channel.
		f.Lock()
		ch, ok := f.pending[m.ID]
		delete(f.pending, m.ID)
		f.Unlock()
		if !ok {
			continue
		}
		if m.Error != "" {
			msg := m.Message
			if msg == "" {
				msg = m.Error
			}
			log.Printf("lorca/firefox: BiDi error id=%d error=%q message=%q", m.ID, m.Error, m.Message)
			ch <- result{Err: errors.New(msg)}
		} else {
			ch <- result{Value: m.Result}
		}
	}
}

func (f *firefox) eval(expr string) (json.RawMessage, error) {
	raw, err := f.send("script.evaluate", h{
		"expression":      expr,
		"awaitPromise":    true,
		"target":          h{"context": f.context},
		"resultOwnership": "root",
	})
	if err != nil {
		return nil, err
	}

	var evalResult struct {
		Type             string          `json:"type"`
		Result           json.RawMessage `json:"result"`
		ExceptionDetails struct {
			Text      string `json:"text"`
			Exception struct {
				Type        string          `json:"type"`
				Value       json.RawMessage `json:"value"`
				Description string          `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &evalResult); err != nil {
		return nil, err
	}
	if evalResult.Type == "exception" {
		ex := evalResult.ExceptionDetails.Exception
		if len(ex.Value) > 0 {
			if unwrapped, err2 := bidiValueToJSON(ex.Value); err2 == nil {
				return nil, errors.New(string(unwrapped))
			}
			return nil, errors.New(string(ex.Value))
		}
		if ex.Description != "" {
			return nil, errors.New(ex.Description)
		}
		return nil, errors.New(evalResult.ExceptionDetails.Text)
	}
	if len(evalResult.Result) == 0 {
		return json.RawMessage("null"), nil
	}
	return bidiValueToJSON(evalResult.Result)
}

func (f *firefox) load(url string) error {
	_, err := f.send("browsingContext.navigate", h{
		"url":     url,
		"context": f.context,
		"wait":    "none",
	})
	return err
}

func (f *firefox) injectScript(js string) error {
	preview := js
	if len(preview) > 120 {
		preview = preview[:120] + "..."
	}
	// Do NOT register this script via script.addPreloadScript.  Preload scripts
	// run in a BiDi sandbox realm, and ALL objects created there — including
	// bootstrap objects like window.__lorcaWS, window.__lorcaWS.onopen, and
	// window.__lorcaWS.onmessage — are sandbox-realm objects.  If app code (e.g.
	// Vue's reactivity system) puts any of those objects into a reactive context
	// and checks .length on the event handler functions, Firefox throws
	// "Permission denied to access property 'length'" via Xray.
	//
	// Confirmed dead ends:
	//   - script.addPreloadScript directly — sandbox-realm
	//   - <script> element appended from preload sandbox — also sandbox-realm
	//   - window.eval(), new window.Function() from preload — sandbox-realm
	//   - exportFunction — not available in BiDi preloads
	//   - document.write — aborts browsingContext.navigate with "Address rejected"
	//
	// Instead, store the script in loadScripts.  It will be re-evaluated in the
	// page window realm on every realmCreated / browsingContext.load event, which
	// fires early enough to win the race against page scripts most of the time,
	// and is unconditionally correct after page load completes.
	f.Lock()
	// Prepend so this script runs before any binding scripts (binding scripts
	// reference window.__lorcaWS etc. which must exist first).
	f.loadScripts = append([]string{js}, f.loadScripts...)
	f.Unlock()
	log.Printf("lorca/firefox injectScript eval: %s", preview)
	_, err := f.eval(js)
	if err != nil {
		log.Printf("lorca/firefox injectScript eval error: %v", err)
	}
	return err
}

func (f *firefox) injectBinding(name string) error {
	code := bindingScript(name)

	// Do NOT inject binding functions via script.addPreloadScript.  Preload
	// scripts run in a BiDi sandbox realm isolated from the page realm via
	// Firefox's Xray wrappers.  Any function objects created in that sandbox
	// cause "Permission denied to access property 'length'" when page-realm
	// code (e.g. Vue's reactivity system) introspects them.  This applies even
	// to <script> elements appended from the sandbox (also sandbox-realm per
	// confirmed testing) and to the bootstrap itself.
	//
	// Binding scripts are stored in loadScripts (alongside the bootstrap, which
	// is stored there by injectScript).  They are installed in page-realm by two
	// mechanisms:
	//   1. script.realmCreated handler: fires sendNoWait for all loadScripts
	//      as soon as the real-origin window realm is created.  Wins the race vs
	//      Vue most of the time; if it loses, Vue sees undefined (no error).
	//   2. browsingContext.load handler: unconditional page-realm eval of all
	//      loadScripts after every page load (belt-and-suspenders).

	// Store for both realmCreated early-eval and post-load re-eval.
	f.Lock()
	f.loadScripts = append(f.loadScripts, code)
	f.Unlock()

	// Fire-and-forget immediate eval on the current page so the binding is
	// available right away without blocking the caller for a round trip.
	// With 50+ bindings registered at startup, blocking evals add up to
	// 10+ seconds of lag.  Errors are silently dropped; realmCreated /
	// browsingContext.load provide authoritative re-installation on every
	// navigation.
	f.sendNoWait("script.evaluate", h{
		"expression":      code,
		"awaitPromise":    false,
		"target":          h{"context": f.context},
		"resultOwnership": "none",
	})
	return nil
}

func (f *firefox) setBounds(b Bounds) error {
	if b.Left != 0 {
		log.Printf("lorca/firefox: SetBounds Left=%d not supported", b.Left)
	}
	if b.Top != 0 {
		log.Printf("lorca/firefox: SetBounds Top=%d not supported", b.Top)
	}
	if b.WindowState != "" && b.WindowState != WindowStateNormal {
		log.Printf("lorca/firefox: SetBounds WindowState=%q not supported", b.WindowState)
	}
	if b.Width > 0 || b.Height > 0 {
		if _, err := f.send("browsingContext.setViewport", h{
			"context":  f.context,
			"viewport": h{"width": b.Width, "height": b.Height},
		}); err != nil {
			return err
		}
		f.Lock()
		f.lastBounds.Width = b.Width
		f.lastBounds.Height = b.Height
		f.Unlock()
	}
	return nil
}

func (f *firefox) bounds() (Bounds, error) {
	raw, err := f.eval("[window.innerWidth, window.innerHeight]")
	if err != nil {
		return Bounds{}, err
	}
	var dims [2]int
	if err := json.Unmarshal(raw, &dims); err != nil {
		return Bounds{}, err
	}
	return Bounds{Width: dims[0], Height: dims[1]}, nil
}

func (f *firefox) kill() {
	log.Printf("lorca/firefox: kill() called")
	if f.bidi != nil {
		f.bidi.Close()
	}
	f.Lock()
	for _, ch := range f.pending {
		ch <- result{Err: errors.New("firefox closed")}
	}
	f.pending = map[int]chan result{}
	f.Unlock()

	if state := f.cmd.ProcessState; state == nil || !state.Exited() {
		killProcessTree(f.cmd.Process.Pid)
	}
}

func (f *firefox) done() <-chan struct{} { return f.doneC }
