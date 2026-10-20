# Development rules

- Do not modify `README.md` as part of a code change. Avoid changing it at all. If a change seems necessary, stop and ask a human for approval before editing it.
- Do not add documentation files anywhere in the repo (design docs, plans, notes, dev logs, ADRs, extra READMEs, or similar). A human must confirm any documentation before it is created or changed.
- The code is the source of truth. When behaviour changes, update code, tests, and code comments, not docs.
- Before finishing a Go change, run `go build ./...`, `go vet ./...`, and `go test ./...`. If you change `apps/web`, also run `pnpm --dir apps/web test`.
