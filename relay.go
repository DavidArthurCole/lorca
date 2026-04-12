package lorca

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/net/websocket"
)

// bindingScript returns the JS that installs window[name] as a relay-backed
// binding function.  The generated code is safe to inject in any realm
// (Chrome page realm via Page.addScriptToEvaluateOnNewDocument, or Firefox
// page realm via script.evaluate / post-load re-eval).
//
// On Firefox, binding functions MUST be installed in the page realm (not the
// BiDi preload sandbox realm) because page-realm code (e.g. Vue's reactivity
// system) checks .length on functions it encounters, and accessing .length
// on a sandbox-realm function throws "Permission denied to access property
// 'length'" via Firefox's Xray wrapper.
//
// The send logic is inlined here (rather than delegating to window.__lorcaSend)
// so that the binding function itself contains no references to sandbox-realm
// function objects.  window.__lorcaWS.send() and window.__lorcaQueue.push()
// are method calls on Xray-wrapped objects, which Firefox permits.
func bindingScript(name string) string {
	body := `var args = Array.prototype.slice.call(arguments); ` +
		`var seq = (window['` + name + `']._seq = (window['` + name + `']._seq || 0) + 1); ` +
		`return new Promise(function(resolve, reject) { ` +
		`window.__lorcaPending.set('` + name + `:' + seq, {resolve: resolve, reject: reject}); ` +
		`var _m = JSON.stringify({name: '` + name + `', seq: seq, args: args}); ` +
		`if (window.__lorcaOpen) { window.__lorcaWS.send(_m); } else { window.__lorcaQueue.push(_m); } ` +
		`});`
	return `window['` + name + `'] = function() { ` + body + ` }; window['` + name + `']._seq = 0;`
}

// bootstrapTemplate is the JS code injected into every page to set up the
// relay WebSocket and the lorca messaging primitives.
//
// For the Firefox preload path (script.addPreloadScript in firefox.go) this
// template runs in the BiDi preload sandbox realm.  All objects created here
// are sandbox-realm objects accessible from page realm via Xray wrappers.
//
// IMPORTANT: window.__lorcaSend has been intentionally removed.  On Firefox,
// the Xray wrapper allows METHOD CALLS on sandbox-realm objects (e.g.
// window.__lorcaWS.send(), window.__lorcaQueue.push()) but throws
// "Permission denied to access property 'length'" when page-realm code
// introspects a sandbox-realm FUNCTION object.  bindingScript() inlines the
// send logic to avoid any reference to a sandbox-realm function, using only
// allowed method calls through Xray.
//
// window.__lorcaWS, __lorcaPending, __lorcaQueue, __lorcaOpen are all
// non-function objects/primitives -Vue's reactivity system only checks
// .length on typeof-function values, so these are safe as sandbox-realm.
const bootstrapTemplate = `(function() {
  var _proto = window.location && window.location.protocol
  if (_proto && _proto !== 'http:' && _proto !== 'https:' && _proto !== 'data:') { return }
  if (_proto !== 'data:') { var _orig = window.location.origin; if (!_orig || _orig === 'null') { return } }
  if (window.__lorcaWS && window.__lorcaWS.readyState <= 1) { return }
  window.__lorcaWS = new WebSocket('ws://127.0.0.1:__RELAY_PORT__')
  window.__lorcaPending = new Map()
  window.__lorcaQueue = []
  window.__lorcaOpen = false
  window.__lorcaWS.onopen = function() { window.__lorcaOpen = true; for (var i = 0; i < window.__lorcaQueue.length; i++) { window.__lorcaWS.send(window.__lorcaQueue[i]) } window.__lorcaQueue = [] }
  window.__lorcaWS.onmessage = function(e) { var msg = JSON.parse(e.data); if (msg.type === 'result') { var cb = window.__lorcaPending.get(msg.name + ':' + msg.seq); if (cb) { if (msg.error) { cb.reject(new Error(msg.error)); } else { cb.resolve(msg.result); } window.__lorcaPending.delete(msg.name + ':' + msg.seq); } } }
})()`

type relay struct {
	mu       sync.Mutex
	bindings map[string]bindingFunc
	names    []string
	client   *websocket.Conn
	port     int
	ln       net.Listener
	server   *http.Server
}

func newRelay() (*relay, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &relay{
		port:     ln.Addr().(*net.TCPAddr).Port,
		ln:       ln,
		bindings: map[string]bindingFunc{},
	}
	// Use a custom Server with no origin check so that pages loaded from
	// data: URIs (null origin) and file: URIs can connect to the relay.
	wsServer := websocket.Server{
		Handler: websocket.Handler(r.handleClient),
		Handshake: func(cfg *websocket.Config, req *http.Request) error {
			return nil // accept any origin
		},
	}
	mux := http.NewServeMux()
	mux.Handle("/", wsServer)
	r.server = &http.Server{Handler: mux}
	go r.server.Serve(ln)
	return r, nil
}

func (r *relay) bootstrapScript() string {
	return strings.ReplaceAll(bootstrapTemplate, "__RELAY_PORT__", fmt.Sprintf("%d", r.port))
}

// bind registers name → f. If name already exists, only the handler is
// updated (no register message is sent -a second register would reset the
// JS-side seq counter). All writes to client happen under mu to serialise
// with the replay in handleClient.
func (r *relay) bind(name string, f bindingFunc) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, exists := r.bindings[name]
	r.bindings[name] = f
	if exists {
		return nil
	}
	r.names = append(r.names, name)
	if r.client != nil {
		return websocket.JSON.Send(r.client, map[string]string{"type": "register", "name": name})
	}
	return nil
}

func (r *relay) handleClient(ws *websocket.Conn) {
	// Swap in new client and replay all registered bindings under the lock.
	// Holding the lock during replay (a few loopback writes) prevents bind()
	// racing to send a duplicate register for a name that is also in the list.
	r.mu.Lock()
	old := r.client
	r.client = ws
	log.Printf("lorca/relay: page connected, replaying %d binding(s)", len(r.names))
	for _, name := range r.names {
		if err := websocket.JSON.Send(ws, map[string]string{"type": "register", "name": name}); err != nil {
			log.Printf("lorca/relay: send register(%s) failed: %v", name, err)
			r.mu.Unlock()
			if old != nil {
				old.Close()
			}
			return
		}
	}
	r.mu.Unlock()

	if old != nil {
		old.Close()
	}

	type callMsg struct {
		Name string            `json:"name"`
		Seq  int               `json:"seq"`
		Args []json.RawMessage `json:"args"`
	}
	type resultMsg struct {
		Type   string           `json:"type"`
		Name   string           `json:"name"`
		Seq    int              `json:"seq"`
		Result *json.RawMessage `json:"result,omitempty"`
		Error  string           `json:"error,omitempty"`
	}

	for {
		var call callMsg
		if err := websocket.JSON.Receive(ws, &call); err != nil {
			log.Printf("lorca/relay: client read error (disconnect?): %v", err)
			break
		}
		log.Printf("lorca/relay: call %s seq=%d", call.Name, call.Seq)
		r.mu.Lock()
		f, ok := r.bindings[call.Name]
		r.mu.Unlock()
		if !ok {
			log.Printf("lorca/relay: no binding for %q", call.Name)
			continue
		}
		name, seq, args := call.Name, call.Seq, call.Args
		go func() {
			msg := resultMsg{Type: "result", Name: name, Seq: seq}
			res, err := f(args)
			if err != nil {
				msg.Error = err.Error()
			} else if b, err2 := json.Marshal(res); err2 != nil {
				msg.Error = err2.Error()
			} else {
				raw := json.RawMessage(b)
				msg.Result = &raw
			}
			r.mu.Lock()
			// Only write if this is still the active client. Results for
			// in-flight calls from a navigated-away page are discarded.
			if r.client == ws {
				websocket.JSON.Send(r.client, msg) //nolint:errcheck
			}
			r.mu.Unlock()
		}()
	}

	r.mu.Lock()
	if r.client == ws {
		r.client = nil
	}
	r.mu.Unlock()
}

func (r *relay) close() {
	r.server.Close()
	r.mu.Lock()
	client := r.client
	r.client = nil
	r.mu.Unlock()
	if client != nil {
		client.Close()
	}
}
