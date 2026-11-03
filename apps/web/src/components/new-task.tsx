import { useEffect, useRef, useState, type FormEvent } from "react";
import { ChevronDown, Settings } from "lucide-react";
import { Button } from "@makez/ui/components/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuTrigger,
} from "@makez/ui/components/dropdown-menu";
import { Input } from "@makez/ui/components/input";
import { createTask, type Agent, type Task, type Workspace } from "@/lib/api";
import { message } from "@/lib/format";
import { PromptComposer, promptSuggestions } from "@/components/prompt-composer";

export function NewTask({
  token,
  readOnly = false,
  agents,
  draft,
  onDraftChange,
  connectError,
  workspaces,
  workspaceID,
  onWorkspace,
  onOpenSettings,
  onCreated,
}: {
  token: string;
  readOnly?: boolean;
  agents: Agent[];
  draft: string;
  onDraftChange: (value: string) => void;
  connectError: string;
  workspaces: Workspace[];
  workspaceID: string;
  onWorkspace: (id: string) => void;
  onOpenSettings: () => void;
  onCreated: (task: Task) => void;
}) {
  const preferred = agents.find((item) => item.ID === "codex") ?? agents[0];
  const preferredValue = preferred ? `${preferred.ID}@${preferred.Version}` : "";
  const [agent, setAgent] = useState(preferredValue);
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
    if (busy || readOnly) return;
    if (!token) {
      setError("No workspace is available for this account.");
      return;
    }
    if (!agent || !draft.trim()) return;
    if (new TextEncoder().encode(draft).byteLength > 65536) {
      setError("The request cannot exceed 65536 bytes");
      return;
    }
    setBusy(true);
    setError("");
    try {
      const task = await createTask(
        token,
        { prompt: draft, agent, delaySeconds: Number(delay) },
        attempt,
      );
      attempt.current = null;
      onCreated(task);
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  }

  const current = workspaces.find((item) => item.id === workspaceID);

  return (
    <section className="flex min-h-0 flex-1 items-center overflow-auto bg-surface-sunken px-4 py-10">
      <div className="mx-auto flex w-full max-w-[640px] flex-col gap-3">
        <h1 className="sr-only">New task</h1>
        <div className="flex flex-wrap items-center gap-3">
          <DropdownMenu>
            <DropdownMenuTrigger
              render={<Button variant="ghost" aria-label="Workspace" className="h-8 px-2" />}
            >
              <span className="max-w-64 truncate">
                {current ? `${current.slug} · ${current.role}` : "No workspace"}
              </span>
              <ChevronDown />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="start" className="w-64!">
              {workspaces.length ? (
                <DropdownMenuGroup>
                  {workspaces.map((item) => (
                    <DropdownMenuItem
                      key={item.id}
                      data-workspace-id={item.id}
                      onClick={() => onWorkspace(item.id)}
                    >
                      {item.slug} · {item.role}
                    </DropdownMenuItem>
                  ))}
                </DropdownMenuGroup>
              ) : (
                <DropdownMenuGroup>
                  <DropdownMenuLabel className="whitespace-normal">
                    No workspace yet.
                  </DropdownMenuLabel>
                  <DropdownMenuItem onClick={onOpenSettings}>
                    <Settings />
                    Open Settings
                  </DropdownMenuItem>
                </DropdownMenuGroup>
              )}
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
        <form onSubmit={submit} className="flex flex-col gap-3">
          <PromptComposer
            id="prompt"
            multiline
            autoFocus
            value={draft}
            onValueChange={onDraftChange}
            placeholder="Describe the goal and the PR or artifact you want."
            agent={agent}
            agents={agents}
            onAgentChange={setAgent}
            busy={busy}
            sendDisabled={readOnly || !agent || !token}
          />
          {agent.startsWith("demo@") && (
            <div className="flex flex-col gap-2">
              <div className="flex items-center gap-2 text-sm text-muted-foreground">
                <label htmlFor="delay">Demo duration</label>
                <Input
                  id="delay"
                  type="number"
                  min={0}
                  max={300}
                  step={1}
                  required
                  value={delay}
                  onChange={(event) => setDelay(event.target.value)}
                  disabled={busy}
                  className="w-20"
                />
                <span>seconds</span>
              </div>
              <p className="text-xs text-muted-foreground">
                Demo checks the run and delivery flow. It does not call a model.
              </p>
            </div>
          )}
          <div className="flex flex-wrap gap-2">
            {promptSuggestions.map((item) => (
              <Button
                key={item.label}
                type="button"
                variant="outline"
                size="sm"
                className="rounded-full"
                onClick={() => onDraftChange(item.prompt)}
              >
                {item.label}
              </Button>
            ))}
          </div>
        </form>
        {readOnly && (
          <p className="text-sm text-muted-foreground">
            This workspace is read-only. Choose another workspace to create tasks.
          </p>
        )}
        {(error || connectError) && (
          <p className="text-sm text-destructive" role="alert">
            {error || connectError}
          </p>
        )}
        {token && !agents.length && (
          <p className="text-sm text-destructive" role="alert">
            This server has no agents registered.
          </p>
        )}
      </div>
    </section>
  );
}
