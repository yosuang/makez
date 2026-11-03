import { Button } from "@makez/ui/components/button";
import { cn } from "@makez/ui/lib/utils";
import markSvg from "@/assets/mark.svg?raw";

export function LoginPage({
  pending = false,
  error = "",
  onSignIn,
}: {
  pending?: boolean;
  error?: string;
  onSignIn: () => void;
}) {
  return (
    <main className="flex min-h-svh flex-col bg-background text-foreground">
      <div className="flex items-center gap-2 px-6 py-5">
        <Mark className="size-5" />
        <span className="text-sm font-medium tracking-tight">Makez</span>
      </div>
      <div className="flex flex-1 items-center justify-center px-6">
        <div className="flex w-full max-w-sm flex-col items-center gap-6 text-center">
          <div className="flex flex-col gap-2">
            <h1 className="text-2xl font-medium tracking-tight">Welcome to Makez</h1>
            <p className="text-sm text-muted-foreground">Make building easy.</p>
          </div>
          <Button className="w-full" disabled={pending} onClick={onSignIn}>
            <GoogleMark />
            Sign in with Google
          </Button>
          {error ? (
            <p className="text-sm text-destructive" role="alert">
              {error}
            </p>
          ) : null}
        </div>
      </div>
      <p className="px-6 py-8 text-center text-xs text-muted-foreground">
        Terms of Service and Privacy Policy
      </p>
    </main>
  );
}

function Mark({ className }: { className?: string }) {
  return (
    <span
      aria-hidden="true"
      className={cn("inline-block text-foreground [&_svg]:block [&_svg]:size-full", className)}
      dangerouslySetInnerHTML={{ __html: markSvg }}
    />
  );
}

function GoogleMark() {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true" data-icon="inline-start">
      <path
        fill="currentColor"
        d="M21.35 11.1h-9.17v2.73h5.51c-.24 1.48-1.73 4.35-5.51 4.35-3.31 0-6.02-2.74-6.02-6.12s2.71-6.12 6.02-6.12c1.88 0 3.14.8 3.86 1.48l2.63-2.54C17.18 3.4 14.9 2.4 12.18 2.4 6.98 2.4 2.76 6.62 2.76 11.82s4.22 9.42 9.42 9.42c5.44 0 9.03-3.82 9.03-9.2 0-.62-.07-1.09-.16-1.54z"
      />
    </svg>
  );
}
