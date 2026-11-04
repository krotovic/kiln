package lsp_test

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/krotovic/kiln/language"
	"github.com/krotovic/kiln/lsp"
	"github.com/krotovic/kiln/lsp/lsptest"
)

func TestStreamsAreFramedByContentLength(t *testing.T) {
	in := strings.NewReader("Content-Length: 2\r\n\r\n{}" +
		"content-type: application/vscode-jsonrpc; charset=utf-8\r\ncontent-length: 7\r\n\r\n[1,2,3]" +
		"Content-Length: 1\r\n\r\n")
	var out bytes.Buffer
	c := lsp.NewStreamConn(in, &out)
	for _, want := range []string{"{}", "[1,2,3]"} {
		got, err := c.ReadMessage()
		if err != nil || string(got) != want {
			t.Fatalf("read %q, %v; want %q", got, err, want)
		}
	}
	if _, err := c.ReadMessage(); err == nil {
		t.Fatal("a message cut short was read")
	}
	if err := c.WriteMessage([]byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Content-Length: 7\r\n\r\n{\"a\":1}" {
		t.Fatalf("wrote %q", out.String())
	}
	_ = c.Close()
	if err := c.WriteMessage([]byte("{}")); err == nil {
		t.Fatal("a closed stream was written")
	}
	for _, bad := range []string{"Content-Type: x\r\n\r\n{}", "Content-Length: -1\r\n\r\n", "Content-Length: many\r\n\r\n"} {
		if _, err := lsp.NewStreamConn(strings.NewReader(bad), io.Discard).ReadMessage(); err == nil {
			t.Fatalf("%q was read", bad)
		}
	}
}

// stream serves a session on pipes and opens its client; done closes once the session ends.
func stream(t *testing.T, opts lsp.Options) (c *lsptest.Client, done chan struct{}) {
	toServer, fromClient := io.Pipe()
	toClient, fromServer := io.Pipe()
	done = make(chan struct{})
	go func() {
		defer close(done)
		lsp.NewServer(opts).ServeStream(toServer, fromServer)
	}()
	return lsptest.Stream(t, toClient, fromClient), done
}

func TestSessionsOnStreams(t *testing.T) {
	scripts := lsp.NewMemoryScripts(nil)
	c, done := stream(t, lsp.Options{
		Scripts:  scripts,
		Hosts:    []*language.HostModule{host},
		Debounce: 10 * time.Millisecond,
		Watch:    20 * time.Millisecond,
		Scheme:   "alo",
		Notifications: map[string]func(json.RawMessage){
			"alo/scripts": func(raw json.RawMessage) {
				var p struct{ Scripts map[string]string }
				_ = json.Unmarshal(raw, &p)
				scripts.Set(p.Scripts)
			},
		},
	})
	// The host's notifications are taken before initialize, and refused as requests
	c.Notify("alo/scripts", map[string]any{"scripts": map[string]string{"script/util": utilScript}})
	if m := c.Call("alo/scripts", map[string]any{}); m.Error == nil {
		t.Fatalf("a notification as a request: %+v", m)
	}
	if m := c.Initialize(nil); m.Error != nil {
		t.Fatal(m.Error)
	}
	c.Open("alo:/script/greeter", "alo", greeterScript)
	if d := c.Diagnostics("alo:/script/greeter"); len(d) != 0 {
		t.Fatalf("a script importing a stored one has diagnostics: %+v", d)
	}
	// New stored scripts republish the open documents' diagnostics
	c.Notify("alo/scripts", map[string]any{"scripts": map[string]string{}})
	if d := c.Diagnostics("alo:/script/greeter"); len(d) == 0 {
		t.Fatal("a script whose import went has no diagnostics")
	}
	c.Notify("exit", nil)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("exit didn't end the session")
	}
}

func TestStreamSessionsEndWithoutInitialize(t *testing.T) {
	_, done := stream(t, lsp.Options{Scripts: lsp.NewMemoryScripts(nil), InitTimeout: 50 * time.Millisecond})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stream without initialize was held open")
	}
}

func TestMemoryScripts(t *testing.T) {
	m := lsp.NewMemoryScripts(map[string]string{"b": "2", "a": "1"})
	v := m.Version()
	if got := m.List(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("List: %v", got)
	}
	if src, err := m.Load("a"); err != nil || src != "1" {
		t.Fatalf("Load: %q %v", src, err)
	}
	m.Set(map[string]string{"c": "3"})
	if _, err := m.Load("a"); err == nil || m.Version() == v {
		t.Fatalf("Set kept a, or kept the version %d", v)
	}
}
