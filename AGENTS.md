# Development rules

- Do not add documentation files anywhere in the repo (design docs, plans, notes, dev logs, ADRs, extra READMEs, or similar). A human must confirm any new documentation before it is created.
- The code is the source of truth. When behaviour changes, update the code, tests, and code comments together.
- Do not edit `README.md` or any other existing doc yourself. If a change makes one inaccurate, do not leave it inconsistent: point out the exact inconsistency, propose the specific edit, and wait for human approval before applying it.
- Before finishing a Go change, run `go build ./...`, `go vet ./...`, and `go test ./...`. If you change `apps/web`, also run `pnpm --dir apps/web test`.

## Code standards

Write and review changes against the [code standards](docs/code-standards.md).
