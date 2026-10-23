<div align="center">
  <a href="https://makez.dev">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="build/icon/makez-dark.svg" />
      <img src="build/icon/makez-light.svg" width="120" alt="makez" />
    </picture>
  </a>
  <h1>makez</h1>
  <p><b>Make building easy.</b> An AI-powered development platform for turning ideas into products.</p>
</div>

## Direction

- **Agent-agnostic** — Integrate existing and custom agents without depending on a single agent implementation.
- **Cloud execution** — Run agents beyond the constraints of local computing resources.
- **Creation first** — Build around product development and tangible outcomes.

## Local development

Run `make dev` to start the dependencies, backend, and frontend. The launcher defaults
`MAKEZ_ENV` to `development` after loading `.local/env`, enabling Swagger UI at
<http://127.0.0.1:8088/swagger/index.html> with the default backend address. Swagger
remains restricted to loopback clients. Explicit `MAKEZ_ENV` values from the shell
or `.local/env` are preserved, including an empty value; for example,
`MAKEZ_ENV=production make dev` disables development features unless `.local/env`
overrides it. Starting the `makez` binary directly still requires
`MAKEZ_ENV=development` to enable development features.
