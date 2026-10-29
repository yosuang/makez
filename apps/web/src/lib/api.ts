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

export interface Workspace {
  id: string;
  slug: string;
  role: "Admin" | "Operator" | "Viewer";
  authz_version: number;
}
export interface User {
  id: string;
  display_name: string;
}
let access = "";
let expiresAt = 0;
let selected: Workspace | undefined;
let scope = "";
let revision = 0;
let refreshFlight: Promise<void> | undefined;
const sessionEvents = new EventTarget();
const channel =
  typeof BroadcastChannel === "undefined" ? undefined : new BroadcastChannel("makez.session");

// Remove credentials written by older consoles; they are never promoted into a user session.
try {
  localStorage.removeItem(tokenKey);
  localStorage.removeItem(legacyConnectionKey);
} catch {
  /* Storage can be unavailable. */
}

export function onSessionChange(listener: () => void) {
  sessionEvents.addEventListener("change", listener);
  return () => sessionEvents.removeEventListener("change", listener);
}
function clearSession() {
  access = "";
  expiresAt = 0;
  selected = undefined;
  scope = "";
  sessionEvents.dispatchEvent(new Event("change"));
}
channel?.addEventListener("message", (event) => {
  if (event.data === "logout") clearSession();
});
export function sessionScope() {
  return scope;
}
export function currentWorkspace() {
  return selected;
}
export function canWrite() {
  return Boolean(selected && selected.role !== "Viewer");
}
export function selectWorkspace(workspace: Workspace) {
  selected = workspace;
  scope = `scope-${++revision}`;
  sessionEvents.dispatchEvent(new Event("change"));
  return scope;
}
export function signIn() {
  window.location.assign(apiURL("/v1/auth/google/start"));
}

async function cookieRequest(path: string, authorization = "") {
  const headers = new Headers({ "X-Makez-CSRF": "1" });
  if (authorization) headers.set("Authorization", `Bearer ${authorization}`);
  return fetch(apiURL(path), { method: "POST", credentials: "include", headers });
}
export function refreshSession(): Promise<void> {
  if (refreshFlight) return refreshFlight;
  refreshFlight = (async () => {
    // Hold a browser-wide lock until Set-Cookie has completed. Every tab then sends
    // the latest cookie, avoiding accidental replay of a rotated refresh token.
    if (!navigator.locks)
      throw new APIError("This browser cannot safely coordinate sign-in across tabs.", 401);
    await navigator.locks.request("makez.refresh", async () => {
      const response = await cookieRequest("/v1/auth/refresh");
      if (!response.ok) throw new APIError("Please sign in again.", response.status);
      const pair = (await response.json()) as { access_token: string; expires_in: number };
      access = pair.access_token;
      expiresAt = Date.now() + pair.expires_in * 1000;
    });
  })()
    .catch((err) => {
      clearSession();
      channel?.postMessage("logout");
      throw err;
    })
    .finally(() => {
      refreshFlight = undefined;
    });
  return refreshFlight;
}
export async function logout(all = false) {
  if (all && expiresAt <= Date.now()) await refreshSession();
  if (!navigator.locks) throw new APIError("Browser session coordination is unavailable.", 401);
  await navigator.locks.request("makez.refresh", async () => {
    const response = await cookieRequest(
      all ? "/v1/auth/logout-all" : "/v1/auth/logout",
      all ? access : "",
    );
    if (!response.ok) throw new APIError("Sign out failed. Try again.", response.status);
    clearSession();
    channel?.postMessage("logout");
  });
}
export async function sessionProfile(): Promise<{ user: User; workspaces: Workspace[] }> {
  if (!access || expiresAt - Date.now() < 30_000) await refreshSession();
  const headers = { Authorization: `Bearer ${access}` };
  const responses = await Promise.all(
    ["/v1/me", "/v1/workspaces"].map((path) => fetch(apiURL(path), { headers })),
  );
  if (responses.some((r) => !r.ok)) {
    clearSession();
    throw new APIError("Please sign in again.", 401);
  }
  const [user, workspaces] = await Promise.all(responses.map((r) => r.json()));
  return { user, workspaces };
}

export function apiURL(path: string) {
  const base = new URL(apiBaseURL);
  const prefix = base.pathname.replace(/\/+$/, "");
  const suffix = path.startsWith("/") ? path : `/${path}`;
  return `${base.origin}${prefix}${suffix}`;
}

export async function api(requestScope: string, path: string, options: RequestInit = {}) {
  if (!requestScope || requestScope !== scope)
    throw new DOMException("Workspace changed", "AbortError");
  if (!access || expiresAt - Date.now() < 30_000) await refreshSession();
  if (requestScope !== scope) throw new DOMException("Workspace changed", "AbortError");
  const headers = new Headers(options.headers);
  headers.set("Authorization", `Bearer ${access}`);
  if (selected) headers.set("X-Makez-Workspace-ID", selected.id);
  if (options.body) headers.set("Content-Type", "application/json");
  let response: Response;
  try {
    response = await fetch(apiURL(path), { ...options, headers });
  } catch (err) {
    if (options.signal?.aborted) throw err;
    throw new APIError("Cannot reach the makez server. Try again.", 0);
  }
  if (requestScope !== scope) {
    await response.body?.cancel();
    throw new DOMException("Workspace changed", "AbortError");
  }
  if (!response.ok) {
    if (response.status === 401) clearSession();
    if ([403, 404].includes(response.status)) {
      selected = undefined;
      scope = "";
      sessionEvents.dispatchEvent(new Event("change"));
    }
    const body = (await response.json().catch(() => ({}))) as { error?: string };
    throw new APIError(body.error || `Request failed (${response.status})`, response.status);
  }
  // Never replay writes after refresh or authentication failures.
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
