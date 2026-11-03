import { useEffect, useRef, useState } from "react";
import { ArrowUp, Bot, LoaderCircle, Mic, Plus } from "lucide-react";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@makez/ui/components/dropdown-menu";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
  InputGroupTextarea,
} from "@makez/ui/components/input-group";
import { cn } from "@makez/ui/lib/utils";
import type { Agent } from "@/lib/api";

export const promptSuggestions = [
  {
    label: "Review a change",
    prompt: "Review the latest change and list what to fix.",
  },
  {
    label: "Draft a small feature",
    prompt: "Draft a small feature and name the artifact to produce.",
  },
] as const;

type SpeechEvent = {
  resultIndex: number;
  results: ArrayLike<ArrayLike<{ transcript: string }>>;
};

type BrowserSpeech = {
  lang: string;
  interimResults: boolean;
  onresult: ((event: SpeechEvent) => void) | null;
  onerror: (() => void) | null;
  onend: (() => void) | null;
  start: () => void;
  stop: () => void;
};

function recognitionCtor(): (new () => BrowserSpeech) | null {
  if (typeof window === "undefined") return null;
  const host = window as Window & {
    SpeechRecognition?: new () => BrowserSpeech;
    webkitSpeechRecognition?: new () => BrowserSpeech;
  };
  return host.SpeechRecognition ?? host.webkitSpeechRecognition ?? null;
}

export function agentLabel(value: string) {
  if (!value) return "No agent";
  const [id, version] = value.split("@");
  return version ? `${id} · ${version}` : id;
}

export function PromptComposer({
  id,
  value,
  onValueChange,
  placeholder,
  agent,
  agents,
  onAgentChange,
  busy = false,
  sendDisabled = false,
  multiline = false,
  autoFocus = false,
  className,
}: {
  id: string;
  value: string;
  onValueChange: (value: string) => void;
  placeholder: string;
  agent: string;
  agents: Agent[];
  onAgentChange: (value: string) => void;
  busy?: boolean;
  sendDisabled?: boolean;
  multiline?: boolean;
  autoFocus?: boolean;
  className?: string;
}) {
  const valueRef = useRef(value);
  valueRef.current = value;
  const showSend = value.trim().length > 0;
  const menu = (
    <>
      <AddMenu disabled={busy} onPick={(prompt) => onValueChange(prompt)} />
      <AgentMenu agent={agent} agents={agents} disabled={busy} onAgentChange={onAgentChange} />
      <MicButton
        className={multiline ? "ml-auto" : undefined}
        disabled={busy}
        onText={(text) => {
          const current = valueRef.current;
          onValueChange(current.trim() ? `${current.trimEnd()} ${text}` : text);
        }}
      />
      {showSend && (
        <InputGroupButton
          type="submit"
          variant="default"
          size="icon-xs"
          className="rounded-full"
          disabled={busy || sendDisabled}
        >
          {busy ? <LoaderCircle className="animate-spin" /> : <ArrowUp />}
          <span className="sr-only">Send</span>
        </InputGroupButton>
      )}
    </>
  );

  if (!multiline) {
    return (
      <InputGroup className={cn("h-10", className)}>
        <InputGroupAddon align="inline-start">
          <AddMenu disabled={busy} onPick={(prompt) => onValueChange(prompt)} />
        </InputGroupAddon>
        <InputGroupInput
          id={id}
          value={value}
          placeholder={placeholder}
          disabled={busy}
          autoFocus={autoFocus}
          required
          onChange={(event) => onValueChange(event.target.value)}
        />
        <InputGroupAddon align="inline-end">
          <AgentMenu agent={agent} agents={agents} disabled={busy} onAgentChange={onAgentChange} />
          <MicButton
            disabled={busy}
            onText={(text) => {
              const current = valueRef.current;
              onValueChange(current.trim() ? `${current.trimEnd()} ${text}` : text);
            }}
          />
          {showSend && (
            <InputGroupButton
              type="submit"
              variant="default"
              size="icon-xs"
              className="rounded-full"
              disabled={busy || sendDisabled}
            >
              {busy ? <LoaderCircle className="animate-spin" /> : <ArrowUp />}
              <span className="sr-only">Send</span>
            </InputGroupButton>
          )}
        </InputGroupAddon>
      </InputGroup>
    );
  }

  return (
    <InputGroup className={cn("h-auto rounded-xl bg-card shadow-sm", className)}>
      <InputGroupTextarea
        id={id}
        placeholder={placeholder}
        value={value}
        onChange={(event) => onValueChange(event.target.value)}
        disabled={busy}
        autoFocus={autoFocus}
        required
        className="min-h-24 px-4 py-3"
      />
      <InputGroupAddon align="block-end">{menu}</InputGroupAddon>
    </InputGroup>
  );
}

function AddMenu({ disabled, onPick }: { disabled: boolean; onPick: (prompt: string) => void }) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        disabled={disabled}
        render={<InputGroupButton variant="ghost" size="icon-xs" aria-label="Add to prompt" />}
      >
        <Plus />
      </DropdownMenuTrigger>
      <DropdownMenuContent side="top" align="start">
        <DropdownMenuGroup>
          {promptSuggestions.map((item) => (
            <DropdownMenuItem key={item.label} onClick={() => onPick(item.prompt)}>
              {item.label}
            </DropdownMenuItem>
          ))}
        </DropdownMenuGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function AgentMenu({
  agent,
  agents,
  disabled,
  onAgentChange,
}: {
  agent: string;
  agents: Agent[];
  disabled: boolean;
  onAgentChange: (value: string) => void;
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        disabled={disabled || !agents.length}
        render={<InputGroupButton variant="ghost" />}
      >
        <Bot />
        <span className="max-w-40 truncate">{agentLabel(agent)}</span>
      </DropdownMenuTrigger>
      <DropdownMenuContent side="top" align="start">
        <DropdownMenuGroup>
          {agents.map((item) => {
            const value = `${item.ID}@${item.Version}`;
            return (
              <DropdownMenuItem key={value} onClick={() => onAgentChange(value)}>
                {agentLabel(value)}
              </DropdownMenuItem>
            );
          })}
        </DropdownMenuGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function MicButton({
  disabled,
  onText,
  className,
}: {
  disabled: boolean;
  onText: (text: string) => void;
  className?: string;
}) {
  const [supported, setSupported] = useState(false);
  const [listening, setListening] = useState(false);
  const recognition = useRef<BrowserSpeech | null>(null);

  useEffect(() => {
    setSupported(Boolean(recognitionCtor()));
    return () => {
      try {
        recognition.current?.stop();
      } catch {
        // The recognizer is already stopped.
      }
    };
  }, []);

  function toggle() {
    if (!recognition.current) {
      const Ctor = recognitionCtor();
      if (!Ctor) return;
      const next = new Ctor();
      next.lang = "en-US";
      next.interimResults = false;
      next.onresult = (event) => {
        const text = event.results[event.resultIndex]?.[0]?.transcript.trim() ?? "";
        if (text) onText(text);
      };
      next.onerror = () => setListening(false);
      next.onend = () => setListening(false);
      recognition.current = next;
    }
    if (listening) {
      recognition.current.stop();
      setListening(false);
      return;
    }
    try {
      recognition.current.start();
      setListening(true);
    } catch {
      setListening(false);
    }
  }

  return (
    <InputGroupButton
      type="button"
      variant="ghost"
      size="icon-xs"
      className={className}
      aria-label={listening ? "Stop voice input" : "Voice input"}
      aria-pressed={listening}
      title={supported ? "Voice input" : "Voice input is not available in this browser"}
      disabled={disabled || !supported}
      onClick={toggle}
    >
      <Mic />
    </InputGroupButton>
  );
}
