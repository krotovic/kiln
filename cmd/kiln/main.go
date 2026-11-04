// Command kiln is the language for hosts that don't embed the Go package: it compiles scripts against host modules
// given as their definition files, answering in JSON (see docs/compiler.md), and serves the language server over
// its standard input and output (see docs/language-server.md).
//
//	kiln compile < request.json > response.json
//	kiln lsp [flags]
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const usage = `usage: kiln compile < request.json > response.json
       kiln lsp [-host name=file.d.als]... [-scheme kiln] [-language kiln] [-name kiln-lsp] [-module regexp]

compile compiles scripts against host modules given as their definition files. The request and the response are
JSON (see docs/compiler.md in https://github.com/krotovic/kiln).

lsp serves one Language Server Protocol session on its standard input and output (see docs/language-server.md).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "compile":
		if len(os.Args) != 2 {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		err = run(os.Stdin, os.Stdout)
	case "lsp":
		err = serveLSP(os.Args[2:], os.Stdin, os.Stdout, os.Stderr)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "kiln: %v\n", err)
		os.Exit(1)
	}
}

func run(in io.Reader, out io.Writer) error {
	var req Request
	dec := json.NewDecoder(in)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return fmt.Errorf("reading the request: %w", err)
	}
	resp, err := Compile(req)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(resp)
}
