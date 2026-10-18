# makez Web

Independent TanStack Start app on Cloudflare Workers. Vite+ handles development, builds, formatting, lint, and tests. UI primitives come from the official shadcn/ui `base-nova` registry and Base UI.

## Development

Use Node.js 22.18+, 24.11+, or 26+ and the pinned pnpm version in `package.json`.

```sh
pnpm install --frozen-lockfile
pnpm dev
```

Open `http://127.0.0.1:3000`. Start the Go backend separately with `make dev` at the repository root. Enter the workspace API key printed by that command. The key lives only in browser memory; reconnect after a page reload. Disconnect clears the current workspace's task view and stops its polling and event stream.

The default Go API origin is `http://127.0.0.1:8088`. For another local port, copy `.dev.vars.example` to `.dev.vars` and change `MAKEZ_API_ORIGIN`, then restart the Web development server. For example, pair `MAKEZ_LISTEN=127.0.0.1:8089 make dev` with `MAKEZ_API_ORIGIN=http://127.0.0.1:8089`.

`/healthz` checks the Web process only. Workspace connection checks the Go API. Browsers use same-origin `/v1/*` requests; the Worker forwards only the existing task/agent/artifact API routes, the bearer token, idempotency key, and event cursor. SSE and file responses remain streamed. Upstream redirects are rejected, and no cookies or shared backend API key are added. Public upstreams must use HTTPS; HTTP is allowed only for loopback development.

## Checks

```sh
pnpm check
pnpm test
pnpm build
pnpm preview
```

Gateway tests cover route restrictions, authentication, idempotency, streamed SSE, cancellation signals, binary downloads, upstream failures, and redirects. Parser tests cover split UTF-8 and SSE framing. `pnpm build` generates the typed route tree and the Worker deployment in `dist/`; `src/routeTree.gen.ts` is checked in so a fresh checkout can type-check without starting a server.

## Deployment

Create a Worker-accessible HTTPS endpoint for the existing Go API. Keep its bearer authentication enabled. PostgreSQL, Temporal, OpenSandbox, and the backend artifact directory stay behind the Go service. A Cloudflare deployment cannot access your machine's `127.0.0.1`.

```sh
pnpm build
pnpm exec wrangler deploy --var MAKEZ_API_ORIGIN:https://your-go-api.example.com
```

Alternatively, set the production origin in `wrangler.jsonc` before running `pnpm run deploy`. Do not put an API key in that file or any `VITE_*` variable. If the origin is additionally protected by a service-access gateway, configure that integration before deploying; this version forwards workspace bearer authentication only.

## Scope and layout

Source direction: [makez product consensus in Notion](https://app.notion.com/p/yosuang/3fcd231b3a368036917dcbd37e3bc6b2).

The layout takes inspiration from [Orca](https://github.com/stablyai/orca)'s persistent task navigation and [T3 Code](https://github.com/pingdotgg/t3code)'s main activity pane plus contextual results pane. Their product source code and assets are not copied. The first version keeps only the capabilities of the existing Go service:

- Workspace API key connection and dynamically registered Agent selection.
- Task submission with timeout and Demo duration; identical failed submissions reuse their idempotency key until accepted or edited.
- Recent tasks, search, task/run/cleanup status, and cancellation.
- SSE logs with replay on reconnect and cleanup when switching tasks or disconnecting.
- Authorized artifact downloads.
- Responsive navigation and stacked task details on small screens.

The browser retains at most 1000 log events for the selected task and indicates when the limit is reached. The backend retains the event history. Tasks continue running after closing the page.

There is no second business database, D1/Drizzle integration, new account system, terminal, chat follow-up, or object-storage migration in this version. Go remains the authority for tasks, permissions, events, and artifact metadata. Artifact contents still use the existing local archive; a future object store belongs behind the backend artifact module and is independent of Web hosting.
