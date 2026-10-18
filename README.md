<div align="center">
  <h1>makez</h1>
  <p><b>Make building easy.</b> An AI-powered development platform for turning ideas into products.</p>
</div>

## Direction

- **Agent-agnostic** — Integrate existing and custom agents without depending on a single agent implementation.
- **Cloud execution** — Run agents beyond the constraints of local computing resources.
- **Creation first** — Build around product development and tangible outcomes.

## Development

```bash
# Terminal 1: Go API, execution worker, and local infrastructure
make dev

# Terminal 2: independent Web app
make web-install # first run
make web-dev
```

Open **http://127.0.0.1:3000** and connect with the workspace API key printed by `make dev`.
The Go API listens on `127.0.0.1:8088`; it no longer serves a Web page.

The Web app uses TanStack Start, Vite+, and shadcn/ui (Base UI), with a Cloudflare Worker forwarding `/v1/*` to Go. Task state remains in PostgreSQL and artifacts remain in the backend's local archive.

See [Web setup and deployment](apps/web/README.md) and [backend development](docs/implementation-v0.1.md).
