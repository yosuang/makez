import { useEffect, useRef, useState, type FormEvent } from "react";
import { ArrowUp, Bot, Clock, KeyRound, LoaderCircle } from "lucide-react";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@makez/ui/components/dropdown-menu";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
  InputGroupText,
  InputGroupTextarea,
} from "@makez/ui/components/input-group";
import { Separator } from "@makez/ui/components/separator";
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
        <form onSubmit={submit}>
          <InputGroup className="h-auto rounded-xl">
            <InputGroupTextarea
              id="prompt"
              placeholder="Describe the goal and the PR or artifact you want."
              value={draft}
              onChange={(event) => onDraftChange(event.target.value)}
              required
              disabled={busy}
              autoFocus
              className="min-h-36 px-4 py-3"
            />
            <InputGroupAddon align="block-end" className="flex-wrap">
              <DropdownMenu>
                <DropdownMenuTrigger
                  disabled={!token || busy || !agents.length}
                  render={<InputGroupButton variant="ghost" />}
                >
                  <Bot />
                  {agent || "No agent"}
                </DropdownMenuTrigger>
                <DropdownMenuContent side="top" align="start">
                  {agents.map((item) => {
                    const value = `${item.ID}@${item.Version}`;
                    return (
                      <DropdownMenuItem key={value} onClick={() => setAgent(value)}>
                        {value}
                      </DropdownMenuItem>
                    );
                  })}
                </DropdownMenuContent>
              </DropdownMenu>
              <Separator orientation="vertical" className="data-[orientation=vertical]:h-4" />
              <InputGroupText>
                <Clock />
                Time
              </InputGroupText>
              <InputGroupInput
                id="timeout"
                aria-label="Time limit in seconds"
                type="number"
                min={5}
                max={5400}
                step={1}
                required
                value={timeout}
                onChange={(event) => setTimeoutValue(event.target.value)}
                disabled={busy}
                className="h-7 w-14 flex-none px-1"
              />
              <InputGroupText>s</InputGroupText>
              {agent.startsWith("demo@") && (
                <>
                  <Separator orientation="vertical" className="data-[orientation=vertical]:h-4" />
                  <InputGroupText>Demo</InputGroupText>
                  <InputGroupInput
                    id="delay"
                    aria-label="Demo duration in seconds"
                    type="number"
                    min={0}
                    max={300}
                    step={1}
                    required
                    value={delay}
                    onChange={(event) => setDelay(event.target.value)}
                    disabled={busy}
                    className="h-7 w-14 flex-none px-1"
                  />
                  <InputGroupText>s</InputGroupText>
                </>
              )}
              {!token && (
                <InputGroupButton type="button" variant="ghost" onClick={onAccess}>
                  <KeyRound />
                  Access workspace
                </InputGroupButton>
              )}
              <InputGroupButton
                type="submit"
                variant="default"
                size="icon-xs"
                className="ml-auto rounded-full"
                disabled={busy || !token || !agent || !draft.trim()}
              >
                {busy ? <LoaderCircle className="animate-spin" /> : <ArrowUp />}
                <span className="sr-only">Send</span>
              </InputGroupButton>
            </InputGroupAddon>
          </InputGroup>
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
