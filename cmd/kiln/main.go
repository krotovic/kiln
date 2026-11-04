// Command kiln is the compiler for hosts that don't embed the Go package: it compiles scripts against host modules
// given as their definition files, answering in JSON (see docs/compiler.md).
//
//	kiln compile < request.json > response.json
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const usage = `usage: kiln compile < request.json > response.json

Compiles scripts against host modules given as their definition files. The request and the response are JSON
(see docs/compiler.md in https://github.com/krotovic/kiln).
`

func main() {
	if len(os.Args) != 2 || os.Args[1] != "compile" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Stdin, os.Stdout); err != nil {
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
