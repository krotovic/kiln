# Kiln

A statically typed subset of TypeScript for embedding: a Go compiler to WebAssembly with a wazero host (`language/`),
a WebAssembly module writer (`wasm/`), a language server (`lsp/`) and the browser runtime (`js/`, the npm package `@krotovic/kiln`). See
`README.md` and `docs/`.

## Conventions

- Nothing here knows about any host: names, functions and modules come from hosts (`HostModule`).
- Run `go fmt` on all Go code.
- The ABI between compiled modules and hosts lives in `language/abi.go` and is mirrored by `js/src/script.ts`; a change
  to it bumps `abiVersion`, in both.
- Cases in `language/testdata/` are shared by both runtimes: add a case there rather than a test in one runtime only.
- `docs/` describes the language as it is now: a change to its behaviour, APIs or tests updates the matching document
  in the same change.
- Commit messages follow Conventional Commits (`feat:`, `fix:`, `refactor:`, `docs:`, `test:`, `chore:`, `ci:`), with a
  lowercase summary.

## Commands

- Go: `go vet ./... && go test ./...`
- Browser runtime: `npm install`, then `npm run typecheck` and `npm test` (needs Go: it compiles the shared cases first).
