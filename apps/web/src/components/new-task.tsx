import { useEffect, useRef, useState, type FormEvent } from "react";
import { ArrowUp, Bot, Clock, KeyRound, LoaderCircle } from "lucide-react";
import { Button } from "@makez/ui/components/button";
import { Input } from "@makez/ui/components/input";
import { Textarea } from "@makez/ui/components/textarea";
import { api, type Agent, type Task } from "@/lib/api";
import { message } from "@/lib/format";

export function NewTask({
  token,
  agents,
  draft,
  onDraftChange,
  connectError,
  onAccess,
  onCreated,
}: {
  token: string;
  agents: Agent[];
  draft: string;
  onDraftChange: (value: string) => void;
  connectError: string;
  onAccess: () => void;
  onCreated: (task: Task) => void;
}) {
  const preferred = agents.find((item) => item.ID === "codex") ?? agents[0];
  const preferredValue = preferred ? `${preferred.ID}@${preferred.Version}` : "";
  const [agent, setAgent] = useState(preferredValue);
  const [timeout, setTimeoutValue] = useState("600");
  const [delay, setDelay] = useState("10");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const attempt = useRef<{ body: string; key: string } | null>(null);

  useEffect(() => {
    if (agent && agents.some((item) => `${item.ID}@${item.Version}` === agent)) return;
    if (preferredValue) setAgent(preferredValue);
  }, [agent, agents, preferredValue]);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!token) {
      onAccess();
      return;
    }
    if (!agent || !draft.trim()) return;
    if (new TextEncoder().encode(draft).byteLength > 65536) {
      setError("The request cannot exceed 65536 bytes");
      return;
    }
    const [agent_id, agent_version] = agent.split("@");
    const body = JSON.stringify({
      prompt: draft,
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
        await api(token, "/v1/tasks", {
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
    <section className="flex flex-1 overflow-auto px-6 py-10">
      <div className="m-auto w-full max-w-[640px]">
        <h1 className="mb-6 text-center text-2xl font-medium tracking-tight">
          What should we run?
        </h1>
        <form onSubmit={submit} className="rounded-xl border border-border bg-surface-raised">
          <label className="sr-only" htmlFor="prompt">
            Request
          </label>
          <Textarea
            id="prompt"
            placeholder="Describe the goal and the PR or artifact you want."
            value={draft}
            onChange={(event) => onDraftChange(event.target.value)}
            required
            disabled={busy}
            autoFocus
            className="min-h-36 resize-y border-0 bg-transparent px-4 py-4 shadow-none focus-visible:ring-0"
          />
          <div className="flex flex-wrap items-center gap-2 px-3 pb-3">
            <label className="inline-flex h-8 max-w-full items-center gap-1.5 rounded-full border border-border bg-background px-2.5 text-xs text-muted-foreground">
              <Bot className="size-3.5 shrink-0" />
              <span className="sr-only" id="agent-label">
                Agent
              </span>
              <select
                aria-labelledby="agent-label"
                value={agent}
                onChange={(event) => setAgent(event.target.value)}
                disabled={!token || busy}
                className="max-w-44 bg-transparent text-foreground outline-none"
              >
                {!agents.length && <option value="">No agent</option>}
                {agents.map((item) => (
                  <option key={`${item.ID}@${item.Version}`} value={`${item.ID}@${item.Version}`}>
                    {item.ID}@{item.Version}
                  </option>
                ))}
              </select>
            </label>
            <label className="inline-flex h-8 items-center gap-1.5 rounded-full border border-border bg-background px-2.5 text-xs text-muted-foreground">
              <Clock className="size-3.5 shrink-0" />
              Time
              <Input
                id="timeout"
                aria-label="Time limit in seconds"
                type="number"
                min="5"
                max="5400"
                step="1"
                required
                value={timeout}
                onChange={(event) => setTimeoutValue(event.target.value)}
                disabled={busy}
                className="h-6 w-14 border-0 bg-transparent px-1 text-xs shadow-none focus-visible:ring-0"
              />
              <span>s</span>
            </label>
            {agent.startsWith("demo@") && (
              <label className="inline-flex h-8 items-center gap-1.5 rounded-full border border-border bg-background px-2.5 text-xs text-muted-foreground">
                Demo
                <Input
                  id="delay"
                  aria-label="Demo duration in seconds"
                  type="number"
                  min="0"
                  max="300"
                  step="1"
                  required
                  value={delay}
                  onChange={(event) => setDelay(event.target.value)}
                  disabled={busy}
                  className="h-6 w-14 border-0 bg-transparent px-1 text-xs shadow-none focus-visible:ring-0"
                />
                <span>s</span>
              </label>
            )}
            {!token && (
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="rounded-full"
                onClick={onAccess}
              >
                <KeyRound />
                Access workspace
              </Button>
            )}
            <Button
              type="submit"
              size="icon"
              className="ml-auto rounded-full"
              aria-label="Submit task"
              title="Submit task"
              disabled={busy || !token || !agent || !draft.trim()}
            >
              {busy ? <LoaderCircle className="animate-spin" /> : <ArrowUp />}
            </Button>
          </div>
        </form>
        {(error || connectError) && (
          <p className="mt-3 text-sm text-destructive" role="alert">
            {error || connectError}
          </p>
        )}
        {agent.startsWith("demo@") && (
          <p className="mt-3 text-xs text-muted-foreground">
            Demo checks the run and delivery flow. It does not call a model.
          </p>
        )}
        {token && !agents.length && (
          <p className="mt-3 text-sm text-destructive" role="alert">
            This server has no agents registered.
          </p>
        )}
      </div>
    </section>
  );
}
