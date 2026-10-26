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

export const stateLabel = (state?: string) => (state ? stateNames[state] || state : "Not started");

export const message = (err: unknown) =>
  err instanceof Error ? err.message : "Request failed. Try again.";

export function formatTimestamp(value: string) {
  return new Date(value).toLocaleString("en-US", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

export function formatClock(value: string) {
  return new Date(value).toLocaleTimeString("en-US", { hour12: false });
}

export function formatAgo(value: string, now: number) {
  const delta = now - new Date(value).getTime();
  if (!Number.isFinite(delta)) return "";
  const seconds = Math.max(0, Math.floor(delta / 1000));
  if (seconds < 45) return "now";
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours}h`;
  return `${Math.floor(hours / 24)}d`;
}

export function formatWorked(from: string, now: number) {
  const delta = now - new Date(from).getTime();
  if (!Number.isFinite(delta) || delta < 0) return "0s";
  const total = Math.floor(delta / 1000);
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const seconds = total % 60;
  if (hours > 0) return `${hours}h ${minutes}m`;
  if (minutes > 0) return `${minutes}m ${seconds}s`;
  return `${seconds}s`;
}

export function formatBytes(size: number) {
  if (size < 1024) return `${size} B`;
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`;
  return `${(size / (1024 * 1024)).toFixed(1)} MB`;
}
