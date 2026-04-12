package lorca

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/net/websocket"
)

const bootstrapTemplate = `(function() {
  const _ws = new WebSocket('ws://127.0.0.1:__RELAY_PORT__')
  const _pending = new Map()
  var _sendQueue = []
  var _open = false
  function _send(msg) {
    if (_open) { _ws.send(msg) } else { _sendQueue.push(msg) }
  }
  _ws.onopen = function() {
    _open = true
    for (var i = 0; i < _sendQueue.length; i++) { _ws.send(_sendQueue[i]) }
    _sendQueue = []
  }
  function _register(name) {
    window[name] = function() {
      const args = Array.prototype.slice.call(arguments)
      return new Promise(function(resolve, reject) {
        const seq = (window[name]._seq = (window[name]._seq || 0) + 1)
        _pending.set(name + ':' + seq, {resolve: resolve, reject: reject})
        _send(JSON.stringify({name: name, seq: seq, args: args}))
      })
    }
    window[name]._seq = 0
  }
  _ws.onmessage = function(e) {
    const msg = JSON.parse(e.data)
    if (msg.type === 'register') {
      _register(msg.name)
    } else if (msg.type === 'result') {
      const cb = _pending.get(msg.name + ':' + msg.seq)
      if (cb) {
        if (msg.error) { cb.reject(new Error(msg.error)) } else { cb.resolve(msg.result) }
        _pending.delete(msg.name + ':' + msg.seq)
      }
    }
  }
  window.__lorcaRegister = _register
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
// updated (no register message is sent — a second register would reset the
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
	for _, name := range r.names {
		if err := websocket.JSON.Send(ws, map[string]string{"type": "register", "name": name}); err != nil {
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
			break
		}
		r.mu.Lock()
		f, ok := r.bindings[call.Name]
		r.mu.Unlock()
		if !ok {
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
