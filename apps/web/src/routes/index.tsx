import { createFileRoute } from "@tanstack/react-router";
import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import {
  ArrowDown,
  ArrowDownToLine,
  ArrowUp,
  ChevronRight,
  CircleDot,
  File,
  KeyRound,
  Layers2,
  LoaderCircle,
  LogOut,
  Menu,
  Plus,
  Search,
  Square,
  Terminal,
} from "lucide-react";
import { Button } from "@makez/ui/components/button";
import { Input } from "@makez/ui/components/input";
import { Textarea } from "@makez/ui/components/textarea";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@makez/ui/components/dialog";
import {
  api,
  defaultAPIBaseURL,
  eventText,
  readConnection,
  terminal,
  writeConnection,
  type Agent,
  type Artifact,
  type Task,
} from "@/lib/api";
import { useTask } from "@/lib/use-task";

export const Route = createFileRoute("/")({ component: Console });

const stateNames: Record<string, string> = {
  QUEUED: "Queued",
  RUNNING: "Running",
  CANCELING: "Canceling",
  SUCCEEDED: "Succeeded",
  FAILED: "Failed",
  CANCELED: "Canceled",
  PREPARING: "Preparing",
  STARTING: "Starting Agent",
  STOPPING: "Stopping",
  FINALIZING: "Archiving",
  NOT_REQUIRED: "No cleanup",
  PENDING: "Cleanup pending",
  DONE: "Cleaned up",
  BLOCKED: "Cleanup blocked",
};
const label = (state?: string) => (state ? stateNames[state] || state : "Not started");
const message = (err: unknown) =>
  err instanceof Error ? err.message : "Request failed. Try again.";
const date = (value: string) =>
  new Date(value).toLocaleString("en-US", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });

function Status({ state }: { state: string }) {
  return (
    <span className={`status status-${state.toLowerCase()}`}>
      <span className="status-dot" />
      {label(state)}
    </span>
  );
}

function Console() {
  const stored = useRef(readConnection());
  const [session, setSession] = useState<{ baseUrl: string; token: string; agents: Agent[] }>();
  const [draft, setDraft] = useState("");
  const [connectOpen, setConnectOpen] = useState(false);
  const [server, setServer] = useState(stored.current.baseUrl);
  const [key, setKey] = useState(stored.current.token);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const connection = useRef<AbortController | null>(null);

  const connectTo = useCallback(async (baseUrl: string, token: string, signal: AbortSignal) => {
    const agents: Agent[] | null = await (
      await api({ baseUrl, token }, "/v1/agents", { signal })
    ).json();
    if (signal.aborted) return;
    writeConnection({ baseUrl, token });
    setSession({ baseUrl, token, agents: agents ?? [] });
    setConnectOpen(false);
  }, []);

  useEffect(() => {
    const token = stored.current.token.trim();
    const baseUrl = stored.current.baseUrl.trim();
    if (!token || !baseUrl) return;
    const controller = new AbortController();
    connection.current = controller;
    setBusy(true);
    void (async () => {
      try {
        await connectTo(baseUrl, token, controller.signal);
      } catch (err) {
        if (!controller.signal.aborted) {
          setError(message(err));
          setConnectOpen(true);
        }
      } finally {
        if (!controller.signal.aborted) setBusy(false);
      }
    })();
    return () => controller.abort();
  }, [connectTo]);

  async function connect(event: FormEvent) {
    event.preventDefault();
    connection.current?.abort();
    const controller = new AbortController();
    connection.current = controller;
    setBusy(true);
    setError("");
    try {
      await connectTo(server.trim(), key.trim(), controller.signal);
    } catch (err) {
      if (!controller.signal.aborted) setError(message(err));
    } finally {
      if (!controller.signal.aborted) setBusy(false);
    }
  }

  return (
    <>
      <Workspace
        key={session ? `${session.baseUrl}\0${session.token}` : "disconnected"}
        baseUrl={session?.baseUrl ?? ""}
        token={session?.token ?? ""}
        agents={session?.agents ?? []}
        draft={draft}
        onDraftChange={setDraft}
        onConnect={() => {
          setError("");
          setConnectOpen(true);
        }}
        onDisconnect={() => {
          writeConnection({ baseUrl: session?.baseUrl || server.trim(), token: "" });
          setKey("");
          setSession(undefined);
        }}
      />
      <Dialog
        open={connectOpen}
        onOpenChange={(open) => {
          setConnectOpen(open);
          if (!open) {
            connection.current?.abort();
            setBusy(false);
          }
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Connect workspace</DialogTitle>
            <DialogDescription>
              Enter the makez server address and workspace API Key. A successful connection stays in
              this browser, and requests go directly to that server.
            </DialogDescription>
          </DialogHeader>
          <form onSubmit={connect} className="connect-form">
            <label htmlFor="server-url">makez server address</label>
            <Input
              id="server-url"
              type="url"
              inputMode="url"
              autoComplete="off"
              spellCheck={false}
              placeholder={defaultAPIBaseURL}
              value={server}
              onChange={(event) => setServer(event.target.value)}
              required
            />
            <label htmlFor="workspace-key">Workspace API Key</label>
            <Input
              id="workspace-key"
              type="password"
              autoComplete="off"
              placeholder="Workspace API Key"
              value={key}
              onChange={(event) => setKey(event.target.value)}
              required
              autoFocus
            />
            {error && (
              <p className="error" role="alert">
                {error}
              </p>
            )}
            <Button type="submit" disabled={busy || !server.trim() || !key.trim()}>
              {busy && <LoaderCircle className="spin" />}Connect
            </Button>
          </form>
        </DialogContent>
      </Dialog>
    </>
  );
}

type DraftProps = { draft: string; onDraftChange: (value: string) => void };

function Workspace({
  baseUrl,
  token,
  agents,
  onConnect,
  onDisconnect,
  draft,
  onDraftChange,
}: DraftProps & {
  baseUrl: string;
  token: string;
  agents: Agent[];
  onConnect: () => void;
  onDisconnect: () => void;
}) {
  const [tasks, setTasks] = useState<Task[]>([]);
  const [active, setActive] = useState("");
  const activeRef = useRef("");
  const [search, setSearch] = useState("");
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [listError, setListError] = useState("");
  const [actionError, setActionError] = useState("");
  const [loading, setLoading] = useState(Boolean(token));
  const [canceling, setCanceling] = useState(false);
  const [downloading, setDownloading] = useState("");
  const { task, events, artifacts, error, streamState, setTask } = useTask(baseUrl, token, active);
  const logs = useRef<HTMLDivElement>(null);
  const follow = useRef(true);
  const [autoScroll, setAutoScroll] = useState(true);

  useEffect(() => {
    if (!token) return;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    async function refresh() {
      try {
        const data: Task[] = await (
          await api({ baseUrl, token }, "/v1/tasks", { signal: controller.signal })
        ).json();
        if (!controller.signal.aborted) {
          setTasks(data);
          setListError("");
        }
      } catch (err) {
        if (!controller.signal.aborted) setListError(message(err));
      } finally {
        if (!controller.signal.aborted) {
          setLoading(false);
          timer = setTimeout(refresh, 5000);
        }
      }
    }
    void refresh();
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [baseUrl, token]);

  useEffect(() => {
    if (follow.current && logs.current) logs.current.scrollTop = logs.current.scrollHeight;
  }, [events]);

  function select(id: string) {
    activeRef.current = id;
    setActive(id);
    setCanceling(false);
    setActionError("");
    setSidebarOpen(false);
    follow.current = true;
    setAutoScroll(true);
  }

  async function cancel() {
    setCanceling(true);
    setActionError("");
    try {
      const updated: Task = await (
        await api({ baseUrl, token }, `/v1/tasks/${active}/cancel`, { method: "POST" })
      ).json();
      if (activeRef.current === active) setTask(updated);
    } catch (err) {
      if (activeRef.current === active) setActionError(message(err));
    } finally {
      if (activeRef.current === active) setCanceling(false);
    }
  }

  async function download(file: Artifact) {
    setDownloading(file.id);
    setActionError("");
    try {
      const blob = await (await api({ baseUrl, token }, `/v1/artifacts/${file.id}/content`)).blob();
      const url = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = url;
      link.download = file.path.split("/").pop() || "artifact";
      link.click();
      setTimeout(() => URL.revokeObjectURL(url), 5000);
    } catch (err) {
      setActionError(message(err));
    } finally {
      setDownloading("");
    }
  }

  const filtered = tasks.filter(
    (item) => item.prompt.toLowerCase().includes(search.toLowerCase()) || item.id.includes(search),
  );
  const current = task ?? tasks.find((item) => item.id === active);
  const selectedError = actionError || error;

  return (
    <div className="app-shell">
      {sidebarOpen && (
        <button
          className="sidebar-scrim"
          aria-label="Close task list"
          onClick={() => setSidebarOpen(false)}
        />
      )}
      <aside className={`sidebar ${sidebarOpen ? "sidebar-open" : ""}`} aria-label="Tasks">
        <div className="brand">
          <img src="/favicon.svg" width="30" height="30" alt="" />
          <span>makez</span>
        </div>
        <Button className="new-task" variant="outline" onClick={() => select("")}>
          <Plus />
          New task
        </Button>
        <div className="task-search">
          <Search size={15} />
          <Input
            aria-label="Search tasks"
            placeholder="Search tasks…"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
          />
        </div>
        <div className="section-label">
          <span>Recent</span>
          {tasks.length > 0 && <span>{tasks.length}</span>}
        </div>
        <nav className="task-list" aria-label="Recent tasks">
          {loading ? (
            <p className="sidebar-empty">Loading tasks…</p>
          ) : !token ? (
            <p className="sidebar-empty">Connect a workspace to see your tasks here.</p>
          ) : !filtered.length ? (
            <p className="sidebar-empty">
              {search ? "No matching tasks" : "No tasks yet. Start something."}
            </p>
          ) : (
            filtered.map((item) => (
              <button
                key={item.id}
                className={`task-item ${active === item.id ? "task-active" : ""}`}
                aria-current={active === item.id ? "page" : undefined}
                onClick={() => select(item.id)}
              >
                <span className={`task-indicator status-${item.state.toLowerCase()}`}>
                  <span className="status-dot" />
                </span>
                <span className="task-summary">
                  <span className="task-name">{item.prompt}</span>
                  <span className="task-subtitle">
                    {item.agent_id}
                    <span>{label(item.state)}</span>
                  </span>
                </span>
              </button>
            ))
          )}
          {listError && (
            <p className="error sidebar-error" role="alert">
              {listError}
            </p>
          )}
        </nav>
        <div className="workspace-footer">
          <div className="workspace-indicator">
            <span className={`connection-dot ${token ? "connected" : ""}`} />
            <div>
              <strong>{token ? "Workspace connected" : "Not connected"}</strong>
              <span>{token ? serverHost(baseUrl) : "Add a server address and API Key"}</span>
            </div>
          </div>
          {token ? (
            <Button
              variant="ghost"
              size="icon"
              title="Disconnect"
              aria-label="Disconnect"
              onClick={onDisconnect}
            >
              <LogOut />
            </Button>
          ) : (
            <Button
              variant="ghost"
              size="icon"
              title="Connect workspace"
              aria-label="Connect workspace"
              onClick={onConnect}
            >
              <KeyRound />
            </Button>
          )}
        </div>
      </aside>
      <main className="main-area">
        <header className="workspace-header">
          <Button
            variant="ghost"
            size="icon"
            className="mobile-menu"
            aria-label="Open task list"
            onClick={() => setSidebarOpen(true)}
          >
            <Menu />
          </Button>
          <div className="breadcrumbs">
            <Layers2 size={16} />
            <span>Workbench</span>
            <ChevronRight size={14} />
            <strong>{active ? "Task" : "New task"}</strong>
          </div>
          {current && (
            <div className="header-right">
              <Status state={current.state} />
            </div>
          )}
        </header>
        {active ? (
          <div className="task-workspace" key={active}>
            <section className="execution-pane" aria-label="Run">
              <div className="request-card">
                <span className="eyebrow">Request</span>
                <h1>{current?.prompt ?? "Loading task…"}</h1>
                {current && (
                  <div className="request-meta">
                    <span>
                      {current.agent_id}@{current.agent_version}
                    </span>
                    <span>{date(current.created_at)}</span>
                  </div>
                )}
              </div>
              {selectedError && (
                <p className="error action-error" role="alert">
                  {selectedError}
                </p>
              )}
              {(current?.reason || current?.dispatch_error) && (
                <p className="error action-error" role="alert">
                  {current.reason || current.dispatch_error}
                </p>
              )}
              <div className="events-header">
                <div>
                  <Terminal size={15} />
                  <h2>Log</h2>
                </div>
                <span className="stream-state">{streamState}</span>
              </div>
              <div
                className="event-log"
                ref={logs}
                role="log"
                aria-label="Run log"
                aria-live="off"
                tabIndex={0}
                onScroll={() => {
                  const element = logs.current;
                  if (element) {
                    follow.current =
                      element.scrollHeight - element.scrollTop - element.clientHeight < 60;
                    setAutoScroll(follow.current);
                  }
                }}
              >
                {!events.length ? (
                  <div className="log-empty">
                    <Terminal size={28} />
                    <p>
                      {current && terminal(current.state)
                        ? "This task has no log"
                        : "Waiting for the Agent's first update"}
                    </p>
                    <span>The run shows up here as it happens</span>
                  </div>
                ) : (
                  events.map((event) => (
                    <div className="log-line" key={event.seq}>
                      <time>
                        {new Date(event.created_at).toLocaleTimeString("en-US", { hour12: false })}
                      </time>
                      <pre>{eventText(event.payload)}</pre>
                    </div>
                  ))
                )}
              </div>
              <div className="execution-footer">
                <span>
                  {events.length >= 1000
                    ? "Showing the latest 1,000 entries"
                    : `${events.length} entries`}
                </span>
                {!autoScroll && (
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => {
                      follow.current = true;
                      setAutoScroll(true);
                      if (logs.current) logs.current.scrollTop = logs.current.scrollHeight;
                    }}
                  >
                    <ArrowDown />
                    Jump to latest
                  </Button>
                )}
                <span>Leaving this page does not stop the task</span>
              </div>
            </section>
            <aside className="inspector" aria-label="Task details">
              <section>
                <h2>
                  <CircleDot size={15} />
                  Status
                </h2>
                <dl className="status-list">
                  <div>
                    <dt>Task</dt>
                    <dd>{current ? <Status state={current.state} /> : "—"}</dd>
                  </div>
                  <div>
                    <dt>Stage</dt>
                    <dd>{label(current?.run?.state)}</dd>
                  </div>
                  <div>
                    <dt>Cleanup</dt>
                    <dd>{label(current?.run?.cleanup_state)}</dd>
                  </div>
                </dl>
                {current && !terminal(current.state) && (
                  <Button
                    variant="outline"
                    className="cancel-task"
                    disabled={canceling || current.cancel_requested}
                    onClick={cancel}
                  >
                    {canceling ? <LoaderCircle className="spin" /> : <Square size={12} />}
                    {current.cancel_requested ? "Canceling…" : "Cancel task"}
                  </Button>
                )}
              </section>
              <section className="artifacts-section">
                <h2>
                  <Layers2 size={15} />
                  Artifacts<span className="count">{artifacts.length}</span>
                </h2>
                {artifacts.length ? (
                  <div className="artifact-list">
                    {artifacts.map((file) => (
                      <div className="artifact" key={file.id}>
                        <File size={19} />
                        <div>
                          <strong title={file.path}>{file.path}</strong>
                          <span>
                            {file.size < 1024
                              ? `${file.size} B`
                              : `${(file.size / 1024).toFixed(1)} KB`}
                          </span>
                        </div>
                        <Button
                          size="icon"
                          variant="ghost"
                          aria-label={`Download ${file.path}`}
                          disabled={Boolean(downloading)}
                          onClick={() => download(file)}
                        >
                          {downloading === file.id ? (
                            <LoaderCircle className="spin" />
                          ) : (
                            <ArrowDownToLine />
                          )}
                        </Button>
                      </div>
                    ))}
                  </div>
                ) : (
                  <div className="artifacts-empty">
                    <File size={24} />
                    <p>
                      {current && terminal(current.state)
                        ? "No archived artifacts"
                        : "Artifacts show up here"}
                    </p>
                    <span>Download them after the task is archived</span>
                  </div>
                )}
              </section>
              {current && (
                <section className="task-metadata">
                  <h2>Details</h2>
                  <dl>
                    <dt>Task ID</dt>
                    <dd>{current.id}</dd>
                    <dt>Agent</dt>
                    <dd>
                      {current.agent_id}@{current.agent_version}
                    </dd>
                    <dt>Deadline</dt>
                    <dd>{date(current.deadline)}</dd>
                  </dl>
                </section>
              )}
            </aside>
          </div>
        ) : (
          <NewTask
            draft={draft}
            onDraftChange={onDraftChange}
            baseUrl={baseUrl}
            token={token}
            agents={agents}
            onConnect={onConnect}
            onCreated={(created) => {
              onDraftChange("");
              setTasks((previous) => [
                created,
                ...previous.filter((item) => item.id !== created.id),
              ]);
              select(created.id);
            }}
          />
        )}
      </main>
    </div>
  );
}

function serverHost(baseUrl: string) {
  try {
    return new URL(baseUrl).host;
  } catch {
    return baseUrl;
  }
}

function NewTask({
  baseUrl,
  token,
  agents,
  onConnect,
  onCreated,
  draft: prompt,
  onDraftChange: setPrompt,
}: DraftProps & {
  baseUrl: string;
  token: string;
  agents: Agent[];
  onConnect: () => void;
  onCreated: (task: Task) => void;
}) {
  const preferred = agents.find((agent) => agent.ID === "codex") ?? agents[0];
  const [agent, setAgent] = useState(preferred ? `${preferred.ID}@${preferred.Version}` : "");
  const [timeout, setTimeoutValue] = useState("600");
  const [delay, setDelay] = useState("10");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const attempt = useRef<{ body: string; key: string } | null>(null);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!token) {
      onConnect();
      return;
    }
    if (!agent || !prompt.trim()) return;
    if (new TextEncoder().encode(prompt).byteLength > 65536) {
      setError("The request cannot exceed 65536 bytes");
      return;
    }
    const [agent_id, agent_version] = agent.split("@");
    const body = JSON.stringify({
      prompt,
      agent_id,
      agent_version,
      timeout_seconds: Number(timeout),
      config: agent_id === "demo" ? { delay_seconds: Number(delay) } : {},
    });
    if (attempt.current?.body !== body) attempt.current = { body, key: crypto.randomUUID() };
    setBusy(true);
    setError("");
    try {
      const task = (await (
        await api({ baseUrl, token }, "/v1/tasks", {
          method: "POST",
          body,
          headers: { "Idempotency-Key": attempt.current.key },
        })
      ).json()) as Task;
      attempt.current = null;
      onCreated(task);
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="create-page">
      <div className="create-content">
        <div className="create-intro">
          <h1>Turn an idea into a result.</h1>
        </div>
        <form onSubmit={submit} className="composer">
          <label className="sr-only" htmlFor="prompt">
            Request
          </label>
          <Textarea
            id="prompt"
            placeholder="What should the Agent build? Describe the goal and the result you want."
            value={prompt}
            onChange={(event) => setPrompt(event.target.value)}
            required
            disabled={busy}
          />
          <div className="composer-toolbar">
            <div className="agent-picker">
              <CircleDot size={15} />
              <label className="sr-only" htmlFor="agent">
                Agent
              </label>
              <select
                id="agent"
                value={agent}
                onChange={(event) => setAgent(event.target.value)}
                disabled={!token || busy}
              >
                {!agents.length && <option value="">Connect to choose an Agent</option>}
                {agents.map((item) => (
                  <option key={`${item.ID}@${item.Version}`} value={`${item.ID}@${item.Version}`}>
                    {item.ID}@{item.Version}
                  </option>
                ))}
              </select>
            </div>
            {token ? (
              <Button
                type="submit"
                size="icon"
                className="submit-task"
                aria-label="Submit task"
                title="Submit task"
                disabled={busy || !agent || !prompt.trim()}
              >
                {busy ? <LoaderCircle className="spin" /> : <ArrowUp />}
              </Button>
            ) : (
              <Button type="button" onClick={onConnect}>
                <KeyRound />
                Connect
              </Button>
            )}
          </div>
          <div className="composer-options">
            <label htmlFor="timeout">
              Time limit{" "}
              <Input
                id="timeout"
                type="number"
                min="5"
                max="5400"
                step="1"
                required
                value={timeout}
                onChange={(event) => setTimeoutValue(event.target.value)}
                disabled={busy}
              />
              <span>sec</span>
            </label>
            {agent.startsWith("demo@") && (
              <label htmlFor="delay">
                Demo duration{" "}
                <Input
                  id="delay"
                  type="number"
                  min="0"
                  max="300"
                  step="1"
                  required
                  value={delay}
                  onChange={(event) => setDelay(event.target.value)}
                  disabled={busy}
                />
                <span>sec</span>
              </label>
            )}
          </div>
        </form>
        {error && (
          <p className="error" role="alert">
            {error}
          </p>
        )}
        {agent.startsWith("demo@") && (
          <p className="agent-note">
            Demo checks the run and delivery flow. It does not call a model. Real Agents come from
            the server.
          </p>
        )}
        {token && !agents.length && (
          <p className="error" role="alert">
            This server has no Agents registered.
          </p>
        )}
        <div className="creation-hints">
          <span>Start from a concrete goal</span>
          <div>
            <button
              type="button"
              onClick={() =>
                setPrompt(
                  "Build a to-do page I can open directly. Support adding, completing, and deleting tasks, save them locally, and include a short README.",
                )
              }
            >
              Build a to-do app
            </button>
            <button
              type="button"
              onClick={() =>
                setPrompt(
                  "Build a responsive personal portfolio page with an introduction, selected work, and contact details. Deliver the files and a short README.",
                )
              }
            >
              Build a portfolio
            </button>
          </div>
        </div>
      </div>
    </section>
  );
}
