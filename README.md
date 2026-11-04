# Kiln

A statically typed subset of TypeScript for embedding in hosts: games, tools, anything that runs content it doesn't
trust. It comes in two forms:

- **Scripts** are whole programs (modules with classes, interfaces, functions and statements, which import each
  other), compiled to WebAssembly: one module per script, run on servers with [wazero](https://wazero.io) (Go) or
  [wasmtime](https://wasmtime.dev) (Rust) and in the browser by its own engine, so a script gives the same results
  everywhere.
- **Expressions** are single expressions embedded where content needs a small calculation.

It is **safe** (code reaches only what its host gives it, under budgets of steps, nested calls and memory),
**deterministic** (randomness only from the host's seeded generator), **strictly typed** (no implicit conversions but
joining strings, no `undefined`: every value starts at its type's zero value) and **checked early** (compiled when
content is saved). Hosts embed it with their own names, functions and host modules.

## Layout

| Path | Contents |
| --- | --- |
| `language/` | Go: lexer, parser, checker, the WebAssembly code generator, the wazero host, and the tooling (analysis, completion, signature help, formatting, rename) |
| `language/testdata/` | The shared cases: expressions, scripts, formatting and tooling, run by every runtime |
| `cmd/kiln/` | Go: `kiln compile`, the compiler as a command, for hosts that don't embed the Go package (Rust ones) |
| `lsp/` | Go: a Language Server Protocol server over WebSockets for editors, with `lsptest`, a client for hosts' tests |
| `wasm/` | Go: a WebAssembly module writer with an assembler for the text format |
| `js/` | TypeScript: the browser runtime (`@krotovic/kiln`) |
| `rust/` | Rust: the crate `kiln`, a runtime of compiled scripts on wasmtime and the expression interpreter, for Rust hosts (no compiler: modules come from the Go one) |
| `docs/` | The language reference, expressions, the script runtime, the tooling and the language server |

## Using it

Go:

```sh
go get github.com/krotovic/kiln@latest
```

```go
import "github.com/krotovic/kiln/language"
```

TypeScript, installed from a tag. The package is TypeScript source, for a bundler (Vite, esbuild) or tsx:

```sh
npm install github:krotovic/kiln#v0.1.0
```

```ts
import { loadScript } from "@krotovic/kiln";
```

Rust, from a tag or a commit (the crate is in `rust/`; Cargo finds it):

```toml
kiln = { git = "https://github.com/krotovic/kiln", tag = "v0.1.0" }
```

The compiler as a command, for hosts without Go in their process (see [docs/compiler.md](docs/compiler.md)):

```sh
go install github.com/krotovic/kiln/cmd/kiln@latest
kiln compile < request.json > response.json
```

## Development

```sh
go vet ./... && go test ./...   # the compiler and the wasm writer
npm install && npm test         # the browser runtime, against modules the Go compiler compiles (needs Go)
npm run typecheck
cd rust && cargo clippy --all-targets -- -D warnings && cargo test   # the Rust runtime (needs Go too)
```

Run `go fmt` on all Go code and `cargo fmt` on the Rust. A change to the interface between compiled modules and their
hosts (`language/abi.go`, mirrored by `js/src/script.ts` and `rust/src/script`) is a new `abiVersion`.

## License

MIT, see [LICENSE](LICENSE).
