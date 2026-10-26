import { cn } from "@makez/ui/lib/utils";
import { stateLabel } from "@/lib/format";

const running = new Set([
  "RUNNING",
  "PREPARING",
  "STARTING",
  "STOPPING",
  "FINALIZING",
  "CANCELING",
]);

export function StatusPill({ state }: { state: string }) {
  const failed = state === "FAILED" || state === "BLOCKED";
  return (
    <span
      className={cn(
        "inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium",
        running.has(state) && "border-border bg-muted text-muted-foreground",
        failed && "border-destructive/40 bg-destructive/10 text-destructive",
        !running.has(state) && !failed && "border-border bg-muted text-muted-foreground",
      )}
    >
      {stateLabel(state)}
    </span>
  );
}
