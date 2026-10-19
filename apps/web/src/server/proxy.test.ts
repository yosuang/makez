import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { proxyAPI } from "./proxy";

afterEach(() => vi.unstubAllGlobals());
const request = (path: string, options: RequestInit = {}) =>
  new Request(`https://makez.test${path}`, {
    headers: { Authorization: "Bearer test-token" },
    ...options,
  });

describe("Go API gateway", () => {
  it("only forwards known API routes and requires a bearer token", async () => {
    const fetcher = vi.fn();
    vi.stubGlobal("fetch", fetcher);
    expect(
      (await proxyAPI(request("/v1/tasks", { method: "DELETE" }), "https://api.test")).status,
    ).toBe(404);
    expect((await proxyAPI(request("/v1/admin"), "https://api.test")).status).toBe(404);
    expect((await proxyAPI(request("/v1/tasks", { headers: {} }), "https://api.test")).status).toBe(
      401,
    );
    expect(fetcher).not.toHaveBeenCalled();
  });

  it("preserves submission body, idempotency and status without forwarding cookies", async () => {
    const fetcher = vi.fn().mockResolvedValue(Response.json({ id: "task-1" }, { status: 202 }));
    vi.stubGlobal("fetch", fetcher);
    const input = request("/v1/tasks", {
      method: "POST",
      body: '{"prompt":"hello"}',
      headers: {
        Authorization: "Bearer test-token",
        "Content-Type": "application/json",
        "Idempotency-Key": "same-attempt",
        Cookie: "private=1",
      },
    });
    const response = await proxyAPI(input, "https://api.test");
    expect(response.status).toBe(202);
    const [url, init] = fetcher.mock.calls[0];
    expect(url.toString()).toBe("https://api.test/v1/tasks");
    expect(init.headers.get("Idempotency-Key")).toBe("same-attempt");
    expect(init.headers.get("Authorization")).toBe("Bearer test-token");
    expect(init.headers.has("Cookie")).toBe(false);
    expect(await new Response(init.body).text()).toBe('{"prompt":"hello"}');
  });

  it("streams SSE immediately, preserving cursor and cancellation", async () => {
    const stream = new TransformStream<Uint8Array, Uint8Array>();
    const fetcher = vi
      .fn()
      .mockResolvedValue(
        new Response(stream.readable, { headers: { "Content-Type": "text/event-stream" } }),
      );
    vi.stubGlobal("fetch", fetcher);
    const controller = new AbortController();
    const input = request("/v1/tasks/task-1/events?after=5", {
      headers: { Authorization: "Bearer test-token", "Last-Event-ID": "7" },
      signal: controller.signal,
    });
    const response = await proxyAPI(input, "https://api.test");
    expect(fetcher.mock.calls[0][0].search).toBe("?after=5");
    expect(fetcher.mock.calls[0][1].headers.get("Last-Event-ID")).toBe("7");
    const reader = response.body!.getReader(),
      writer = stream.writable.getWriter();
    const write = writer.write(new TextEncoder().encode("event: status\ndata: {}\n\n"));
    expect(new TextDecoder().decode((await reader.read()).value)).toContain("event: status");
    await write;
    controller.abort();
    expect(fetcher.mock.calls[0][1].signal.aborted).toBe(true);
    await reader.cancel();
  });

  it("preserves artifact bytes, attachment headers and authorization failures", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(new Uint8Array([0, 255, 128]), {
          headers: {
            "Content-Disposition": 'attachment; filename="result.zip"',
            "Content-Type": "application/octet-stream",
          },
        }),
      ),
    );
    const response = await proxyAPI(request("/v1/artifacts/a-1/content"), "https://api.test");
    expect(response.headers.get("Content-Disposition")).toContain("result.zip");
    expect(new Uint8Array(await response.arrayBuffer())).toEqual(new Uint8Array([0, 255, 128]));
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(Response.json({ error: "not found" }, { status: 404 })),
    );
    expect(
      (await proxyAPI(request("/v1/tasks/foreign/artifacts"), "https://api.test")).status,
    ).toBe(404);
  });

  it("rejects unsafe upstream configuration and redirects", async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValue(
        new Response(null, { status: 302, headers: { Location: "https://other.test" } }),
      );
    vi.stubGlobal("fetch", fetcher);
    for (const origin of [
      "http://api.test",
      "https://makez.test",
      "https://user:secret@api.test",
      "https://api.test/path",
      "broken",
    ]) {
      expect((await proxyAPI(request("/v1/tasks"), origin)).status).toBe(503);
    }
    expect(fetcher).not.toHaveBeenCalled();
    expect((await proxyAPI(request("/v1/tasks"), "https://api.test")).status).toBe(502);
    expect(fetcher.mock.calls[0][1].redirect).toBe("manual");
    fetcher.mockRejectedValue(new Error("connection refused with private details"));
    const response = await proxyAPI(request("/v1/tasks"), "http://127.0.0.1:8088");
    expect(response.status).toBe(502);
    expect(await response.text()).not.toContain("private details");
  });
});
