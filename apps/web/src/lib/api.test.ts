import { afterEach, expect, it, vi } from "vite-plus/test";
import {
  APIError,
  api,
  apiURL,
  defaultAPIBaseURL,
  eventText,
  readConnection,
  readEvents,
  terminal,
  writeConnection,
} from "./api";

afterEach(() => vi.unstubAllGlobals());

it("parses UTF-8 and CRLF frames split at every byte, ignoring comments and incomplete frames", async () => {
  const bytes = new TextEncoder().encode(
    ': heartbeat\r\nid: 9\r\nevent: log\r\ndata: {"text":"café"}\r\n\r\nevent: status\ndata: first\ndata: second\n\ndata: unfinished',
  );
  const received: unknown[] = [];
  await readEvents(
    new ReadableStream({
      start(controller) {
        for (const byte of bytes) controller.enqueue(Uint8Array.of(byte));
        controller.close();
      },
    }),
    (event) => received.push(event),
  );
  expect(received).toEqual([
    { type: "log", id: "9", data: '{"text":"café"}' },
    { type: "status", id: "", data: "first\nsecond" },
  ]);
});

it("keeps structured logs visible and recognizes only terminal task states", () => {
  expect(eventText({ text: "hello" })).toBe("hello");
  expect(eventText({ tool: "shell", args: ["ls"] })).toBe('{"tool":"shell","args":["ls"]}');
  expect(eventText(null)).toBe("null");
  expect(terminal("CANCELING")).toBe(false);
  expect(terminal("FAILED")).toBe(true);
});

it("joins the configured server root with API paths and rejects unsafe addresses", () => {
  expect(apiURL("http://127.0.0.1:8088/", "/v1/agents")).toBe("http://127.0.0.1:8088/v1/agents");
  expect(apiURL("https://example.com/makez/", "v1/tasks")).toBe(
    "https://example.com/makez/v1/tasks",
  );
  expect(apiURL("https://example.com/makez?ignored=1#hash", "/v1/agents")).toBe(
    "https://example.com/makez/v1/agents",
  );
  for (const baseUrl of ["", "ftp://example.com", "https://user:secret@example.com", "not a url"]) {
    expect(() => apiURL(baseUrl, "/v1/agents")).toThrow(APIError);
  }
});

it("calls the configured origin with the bearer token and reports API failures", async () => {
  const fetcher = vi.fn().mockResolvedValue(Response.json([{ ID: "demo", Version: "1" }]));
  vi.stubGlobal("fetch", fetcher);
  const response = await api({ baseUrl: "http://127.0.0.1:8088/", token: "secret" }, "/v1/agents", {
    headers: { "Last-Event-ID": "4" },
  });
  expect(fetcher.mock.calls[0][0]).toBe("http://127.0.0.1:8088/v1/agents");
  const init = fetcher.mock.calls[0][1] as RequestInit;
  expect(new Headers(init.headers).get("Authorization")).toBe("Bearer secret");
  expect(new Headers(init.headers).get("Last-Event-ID")).toBe("4");
  expect(await response.json()).toEqual([{ ID: "demo", Version: "1" }]);

  fetcher.mockResolvedValue(Response.json({ error: "invalid API key" }, { status: 401 }));
  await expect(
    api({ baseUrl: "https://api.example", token: "nope" }, "/v1/tasks"),
  ).rejects.toMatchObject({
    message: "invalid API key",
    status: 401,
  });

  fetcher.mockRejectedValue(new TypeError("Failed to fetch"));
  await expect(
    api({ baseUrl: "http://127.0.0.1:8088", token: "secret" }, "/v1/agents"),
  ).rejects.toMatchObject({
    message: "Cannot reach the makez server. Check the address and try again.",
    status: 0,
  });

  const controller = new AbortController();
  controller.abort();
  fetcher.mockRejectedValue(new DOMException("The operation was aborted.", "AbortError"));
  await expect(
    api({ baseUrl: "http://127.0.0.1:8088", token: "secret" }, "/v1/agents", {
      signal: controller.signal,
    }),
  ).rejects.toMatchObject({ name: "AbortError" });
});

it("persists the server address and token in this browser", () => {
  const memory = new Map<string, string>();
  vi.stubGlobal("localStorage", {
    getItem: (key: string) => memory.get(key) ?? null,
    setItem: (key: string, value: string) => {
      memory.set(key, value);
    },
    removeItem: (key: string) => {
      memory.delete(key);
    },
  });
  expect(readConnection()).toEqual({ baseUrl: defaultAPIBaseURL, token: "" });
  writeConnection({ baseUrl: " https://api.example/makez/ ", token: " k " });
  expect(readConnection()).toEqual({ baseUrl: "https://api.example/makez/", token: "k" });
  memory.set("makez.connection", "{");
  expect(readConnection()).toEqual({ baseUrl: defaultAPIBaseURL, token: "" });
  memory.set("makez.connection", JSON.stringify({ baseUrl: 1, token: false }));
  expect(readConnection()).toEqual({ baseUrl: defaultAPIBaseURL, token: "" });
});
