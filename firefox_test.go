package lorca

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func skipIfNoFirefox(t *testing.T) string {
	t.Helper()
	path, ok := os.LookupEnv("LORCAFIREFOX")
	if !ok || path == "" {
		t.Skip("LORCAFIREFOX not set; skipping Firefox integration tests")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("LORCAFIREFOX=%s not found; skipping Firefox integration tests", path)
	}
	return path
}

func TestBidiValueToJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"string", `{"type":"string","value":"hello"}`, `"hello"`},
		{"number int", `{"type":"number","value":42}`, `42`},
		{"number float", `{"type":"number","value":3.14}`, `3.14`},
		{"boolean true", `{"type":"boolean","value":true}`, `true`},
		{"boolean false", `{"type":"boolean","value":false}`, `false`},
		{"null", `{"type":"null"}`, `null`},
		{"undefined", `{"type":"undefined"}`, `null`},
		{"array", `{"type":"array","value":[{"type":"string","value":"a"},{"type":"number","value":1}]}`, `["a",1]`},
		{"object", `{"type":"object","value":[["key",{"type":"string","value":"val"}]]}`, `{"key":"val"}`},
		{"nested", `{"type":"array","value":[{"type":"object","value":[["x",{"type":"number","value":1}]]}]}`, `[{"x":1}]`},
		{"unknown type", `{"type":"symbol"}`, `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := bidiValueToJSON(json.RawMessage(tt.input))
			if err != nil {
				t.Fatal(err)
			}
			if string(result) != tt.expected {
				t.Fatalf("expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestFirefoxNew(t *testing.T) {
	binary := skipIfNoFirefox(t)
	dir := t.TempDir()
	f, err := newFirefoxWithArgs(binary, "--headless", "--profile", dir)
	if err != nil {
		t.Fatal(err)
	}
	f.kill()
	select {
	case <-f.done():
	case <-time.After(5 * time.Second):
		t.Fatal("done() did not close after kill")
	}
}

func TestFirefoxEval(t *testing.T) {
	binary := skipIfNoFirefox(t)
	dir := t.TempDir()
	f, err := newFirefoxWithArgs(binary, "--headless", "--profile", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { f.kill(); <-f.done() }()

	for _, tc := range []struct {
		expr   string
		result string
		errMsg string
	}{
		{expr: `42`, result: `42`},
		{expr: `"hello"`, result: `"hello"`},
		{expr: `2+3`, result: `5`},
		{expr: `[1,2,3]`, result: `[1,2,3]`},
		{expr: `({x:1,y:2})`, result: `{"x":1,"y":2}`},
		{expr: `Promise.resolve(7)`, result: `7`},
		{expr: `throw "fail"`, errMsg: `"fail"`},
	} {
		result, err := f.eval(tc.expr)
		if tc.errMsg != "" {
			if err == nil || err.Error() != tc.errMsg {
				t.Fatalf("%s: expected error %q, got %v", tc.expr, tc.errMsg, err)
			}
		} else if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.expr, err)
		} else if string(result) != tc.result {
			t.Fatalf("%s: expected %s, got %s", tc.expr, tc.result, string(result))
		}
	}
}

func TestFirefoxLoad(t *testing.T) {
	binary := skipIfNoFirefox(t)
	dir := t.TempDir()
	f, err := newFirefoxWithArgs(binary, "--headless", "--profile", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { f.kill(); <-f.done() }()

	if err := f.load("data:text/html,<html><body>Hello</body></html>"); err != nil {
		t.Fatal(err)
	}
	result, err := f.eval(`document.body.innerText`)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != `"Hello"` {
		t.Fatalf("expected %q, got %s", "Hello", result)
	}
}

func TestFirefoxInjectScript(t *testing.T) {
	binary := skipIfNoFirefox(t)
	dir := t.TempDir()
	f, err := newFirefoxWithArgs(binary, "--headless", "--profile", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { f.kill(); <-f.done() }()

	if err := f.injectScript(`window.__injected = 99`); err != nil {
		t.Fatal(err)
	}
	result, err := f.eval(`window.__injected`)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != `99` {
		t.Fatalf("expected 99, got %s", result)
	}
}

func TestFirefoxBounds(t *testing.T) {
	binary := skipIfNoFirefox(t)
	dir := t.TempDir()
	f, err := newFirefoxWithArgs(binary, "--headless", "--profile", dir, "--window-size=800,600")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { f.kill(); <-f.done() }()

	b, err := f.bounds()
	if err != nil {
		t.Fatal(err)
	}
	if b.Width == 0 || b.Height == 0 {
		t.Fatalf("expected non-zero bounds, got %+v", b)
	}
}

func TestFirefoxSetBounds(t *testing.T) {
	binary := skipIfNoFirefox(t)
	dir := t.TempDir()
	f, err := newFirefoxWithArgs(binary, "--headless", "--profile", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { f.kill(); <-f.done() }()

	if err := f.setBounds(Bounds{Width: 1024, Height: 768}); err != nil {
		t.Fatal(err)
	}
	b, err := f.bounds()
	if err != nil {
		t.Fatal(err)
	}
	if b.Width != 1024 || b.Height != 768 {
		t.Fatalf("expected 1024x768, got %dx%d", b.Width, b.Height)
	}
}
