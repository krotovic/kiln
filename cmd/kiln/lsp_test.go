package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krotovic/kiln/lsp"
	"github.com/krotovic/kiln/lsp/lsptest"
)

// hostFile writes a definition file and gives its -host flag.
func hostFile(t *testing.T, name, src string) string {
	path := filepath.Join(t.TempDir(), name+".d.als")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return name + "=" + path
}

func TestLSPFlags(t *testing.T) {
	bell := hostFile(t, "bell", bellHost)
	for _, bad := range [][]string{
		{"-host", "bell"},
		{"-host", "=x.d.als"},
		{"-host", bell, "-host", bell},
		{"-host", "bell=" + filepath.Join(t.TempDir(), "missing.d.als")},
		{"-host", hostFile(t, "broken", "export native function f(: void\n")},
		{"-module", "("},
		{"-nope"},
		{"stray"},
	} {
		if _, err := lspServer(bad, io.Discard); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}

// lspSession runs `kiln lsp args` on pipes and opens its client; done closes once it returns.
func lspSession(t *testing.T, args ...string) (c *lsptest.Client, done chan error) {
	toServer, fromClient := io.Pipe()
	toClient, fromServer := io.Pipe()
	done = make(chan error, 1)
	go func() { done <- serveLSP(args, toServer, fromServer, io.Discard) }()
	return lsptest.Stream(t, toClient, fromClient), done
}

func TestLSPServesASessionOnItsStandardStreams(t *testing.T) {
	req := bellRequest()
	c, done := lspSession(t, "-host", hostFile(t, "bell", bellHost), "-host", hostFile(t, "gong", req.Hosts[1].Source),
		"-scheme", "alo", "-language", "alo-script", "-name", "alo-lsp", "-module", `script/[a-z]+`)
	// The stored scripts come in <scheme>/scripts, before initialize as after it
	c.Notify("alo/scripts", ScriptsParams{Scripts: map[string]string{"script/util": req.Scripts["script/util"]}})
	m := c.Initialize(nil)
	if m.Error != nil || !strings.Contains(string(m.Result), `"alo-lsp"`) {
		t.Fatalf("initialize: %s %+v", m.Result, m.Error)
	}
	c.Open("alo:/script/main", "alo-script", req.Scripts["script/main"])
	if d := c.Diagnostics("alo:/script/main"); len(d) != 0 {
		t.Fatalf("a script importing a stored one and a host module has diagnostics: %+v", d)
	}
	m = c.Call("textDocument/hover", lsptest.Pos("alo:/script/main", 3, 33))
	if !strings.Contains(string(m.Result), "```alo-script") || !strings.Contains(string(m.Result), "log(msg: string): void") {
		t.Fatalf("hover on a host's function: %s %+v", m.Result, m.Error)
	}
	m = c.Call("alo/source", map[string]any{"uri": "alo:/host/gong"})
	var src struct {
		Text     string
		ReadOnly bool
	}
	_ = json.Unmarshal(m.Result, &src)
	if !strings.Contains(src.Text, "interface Gong") || !src.ReadOnly {
		t.Fatalf("alo/source of a host module: %s %+v", m.Result, m.Error)
	}
	// -module decides what names a script
	if m := c.Call("textDocument/hover", lsptest.Pos("alo:/script/Main-2", 0, 0)); m.Error == nil {
		t.Fatal("an ID -module refuses was read")
	}
	// Stored scripts that change republish the diagnostics
	c.Notify("alo/scripts", ScriptsParams{Scripts: map[string]string{}})
	var d []lsp.Diagnostic
	for len(d) == 0 {
		d = c.Diagnostics("alo:/script/main")
	}
	c.Notify("exit", nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exit didn't end the session")
	}
}
