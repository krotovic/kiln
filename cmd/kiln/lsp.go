package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/krotovic/kiln/language"
	"github.com/krotovic/kiln/lsp"
)

// hostFlags are -host name=file.d.als, repeated.
type hostFlags []string

func (h *hostFlags) String() string     { return strings.Join(*h, ",") }
func (h *hostFlags) Set(v string) error { *h = append(*h, v); return nil }

// ScriptsParams is the <scheme>/scripts notification: the stored scripts, by ID, replacing the ones before.
type ScriptsParams struct {
	Scripts map[string]string `json:"scripts"`
}

// lspServer reads the flags and makes the server they describe, over stored scripts its <scheme>/scripts
// notification sets.
func lspServer(args []string, stderr io.Writer) (*lsp.Server, error) {
	fs := flag.NewFlagSet("kiln lsp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var hostArgs hostFlags
	fs.Var(&hostArgs, "host", "a host module, `name=file.d.als`: its name, as scripts import it, and its definition file (repeat for more)")
	scheme := fs.String("scheme", "kiln", "the scheme of documents' URIs, <scheme>:/<module ID>, and of the server's own methods")
	languageID := fs.String("language", "kiln", "the language's name in Markdown code blocks, as the editor registers it")
	name := fs.String("name", "kiln-lsp", "the server's name in initialize's answer")
	module := fs.String("module", "", "a regular expression the whole of a script's ID must match (default: any ID but empty)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	var hosts []*language.HostModule
	seen := map[string]bool{}
	for _, arg := range hostArgs {
		hostName, file, ok := strings.Cut(arg, "=")
		if !ok || hostName == "" || file == "" {
			return nil, fmt.Errorf("-host %q is not name=file.d.als", arg)
		}
		if seen[hostName] {
			return nil, fmt.Errorf("the host module %q is given twice", hostName)
		}
		seen[hostName] = true
		src, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("the host module %q: %w", hostName, err)
		}
		hosts = append(hosts, &language.HostModule{Name: hostName, Source: string(src), Declarations: true})
	}
	for _, h := range hosts {
		if err := h.Check(); err != nil {
			return nil, fmt.Errorf("the host module %q: %w", h.Name, err)
		}
	}
	var valid func(string) bool
	if *module != "" {
		re, err := regexp.Compile(`^(?:` + *module + `)$`)
		if err != nil {
			return nil, fmt.Errorf("-module: %w", err)
		}
		valid = re.MatchString
	}
	scripts := lsp.NewMemoryScripts(nil)
	return lsp.NewServer(lsp.Options{
		Scripts:     scripts,
		Hosts:       hosts,
		ValidModule: valid,
		Scheme:      *scheme,
		LanguageID:  *languageID,
		Name:        *name,
		Notifications: map[string]func(json.RawMessage){
			*scheme + "/scripts": func(raw json.RawMessage) {
				var p ScriptsParams
				if err := json.Unmarshal(raw, &p); err != nil {
					log.Printf("[LSP] %s/scripts: %v", *scheme, err)
					return
				}
				scripts.Set(p.Scripts)
			},
		},
	}), nil
}

// serveLSP serves one session on in and out, logging to stderr.
func serveLSP(args []string, in io.Reader, out io.Writer, stderr io.Writer) error {
	srv, err := lspServer(args, stderr)
	if err != nil {
		return err
	}
	log.SetOutput(stderr)
	srv.ServeStream(in, out)
	return nil
}
