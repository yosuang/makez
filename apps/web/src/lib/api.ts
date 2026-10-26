export interface Agent {
  ID: string;
  Version: string;
}
export interface Task {
  id: string;
  workspace_id: string;
  prompt: string;
  agent_id: string;
  agent_version: string;
  state: string;
  reason?: string;
  dispatch_error?: string;
  created_at: string;
  deadline: string;
  cancel_requested: boolean;
  run?: { id: string; state: string; cleanup_state: string; verified_stopped: boolean };
}
export interface Artifact {
  id: string;
  path: string;
  size: number;
  sha256: string;
  media_type: string;
}
export interface RunEvent {
  seq: number;
  type: string;
  payload: unknown;
  created_at: string;
}

export const terminal = (state: string) => ["SUCCEEDED", "FAILED", "CANCELED"].includes(state);

export class APIError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
  }
}

// Local makez listens here when VITE_MAKEZ_SERVER_URL is unset. The value is the
// server root, not a /v1 path, and is fixed for the built console.
const fallbackAPIBaseURL = "http://127.0.0.1:8088";
const tokenKey = "makez.token";
const legacyConnectionKey = "makez.connection";

function configuredBaseURL(): string {
  const raw = import.meta.env.VITE_MAKEZ_SERVER_URL;
  const trimmed = typeof raw === "string" ? raw.trim() : "";
  return assertBaseURL(trimmed || fallbackAPIBaseURL);
}

function assertBaseURL(value: string): string {
  let base: URL;
  try {
    base = new URL(value);
  } catch {
    throw new Error("VITE_MAKEZ_SERVER_URL is not a valid URL");
  }
  if (base.protocol !== "http:" && base.protocol !== "https:") {
    throw new Error("VITE_MAKEZ_SERVER_URL must use http or https");
  }
  if (base.username || base.password) {
    throw new Error("VITE_MAKEZ_SERVER_URL cannot include a username or password");
  }
  return value;
}

export const apiBaseURL = configuredBaseURL();

export function readToken(): string {
  try {
    const current = localStorage.getItem(tokenKey);
    if (typeof current === "string") return current;
    const legacy = localStorage.getItem(legacyConnectionKey);
    if (!legacy) return "";
    const parsed = JSON.parse(legacy) as { token?: unknown };
    const token = typeof parsed.token === "string" ? parsed.token : "";
    if (token) localStorage.setItem(tokenKey, token);
    // Drop the old record so a stored server address cannot override the build.
    localStorage.removeItem(legacyConnectionKey);
    return token;
  } catch {
    return "";
  }
}

export function writeToken(token: string) {
  try {
    const trimmed = token.trim();
    if (trimmed) localStorage.setItem(tokenKey, trimmed);
    else localStorage.removeItem(tokenKey);
    localStorage.removeItem(legacyConnectionKey);
  } catch {
    // Private mode can block storage. The in-memory session still works for this page.
  }
}

export function apiURL(path: string) {
  const base = new URL(apiBaseURL);
  const prefix = base.pathname.replace(/\/+$/, "");
  const suffix = path.startsWith("/") ? path : `/${path}`;
  return `${base.origin}${prefix}${suffix}`;
}

export async function api(token: string, path: string, options: RequestInit = {}) {
  const headers = new Headers(options.headers);
  headers.set("Authorization", `Bearer ${token}`);
  if (options.body) headers.set("Content-Type", "application/json");
  let response: Response;
  try {
    response = await fetch(apiURL(path), { ...options, headers });
  } catch (err) {
    if (options.signal?.aborted) throw err;
    throw new APIError("Cannot reach the makez server. Try again.", 0);
  }
  if (!response.ok) {
    const body = (await response.json().catch(() => ({}))) as { error?: string };
    throw new APIError(body.error || `Request failed (${response.status})`, response.status);
  }
  return response;
}

export function eventText(payload: unknown): string {
  if (typeof payload === "string") return payload;
  if (payload && typeof payload === "object") {
    const data = payload as Record<string, unknown>;
    for (const key of ["text", "message", "data"]) {
      if (typeof data[key] === "string") return data[key];
    }
  }
  return JSON.stringify(payload) ?? "";
}

// One parser for chunk boundaries, UTF-8, and SSE replay in both browser and tests.
export async function readEvents(
  body: ReadableStream<Uint8Array>,
  onEvent: (event: { type: string; id: string; data: string }) => void,
) {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let match: RegExpExecArray | null;
      while ((match = /\r?\n\r?\n/.exec(buffer))) {
        const message = buffer.slice(0, match.index);
        buffer = buffer.slice(match.index + match[0].length);
        let type = "message",
          id = "";
        const data: string[] = [];
        for (const line of message.split(/\r?\n/)) {
          const colon = line.indexOf(":");
          const field = colon < 0 ? line : line.slice(0, colon);
          const value = colon < 0 ? "" : line.slice(colon + 1).replace(/^ /, "");
          if (field === "event") type = value;
          if (field === "id") id = value;
          if (field === "data") data.push(value);
        }
        if (data.length) onEvent({ type, id, data: data.join("\n") });
      }
    }
  } finally {
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

export function isAbort(err: unknown) {
  return err instanceof Error && err.name === "AbortError";
}
