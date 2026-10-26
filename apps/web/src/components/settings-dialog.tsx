import { useEffect, useRef, useState, type CSSProperties } from "react";
import { SlidersHorizontal, Sun } from "lucide-react";
import { Button } from "@makez/ui/components/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@makez/ui/components/dialog";
import { Input } from "@makez/ui/components/input";
import { Label } from "@makez/ui/components/label";
import {
  Sidebar,
  SidebarContent,
  SidebarGroup,
  SidebarGroupContent,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarProvider,
  SidebarInset,
} from "@makez/ui/components/sidebar";
import { Switch } from "@makez/ui/components/switch";
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@makez/ui/components/breadcrumb";
import { isAbort } from "@/lib/api";
import { message } from "@/lib/format";
import { applyTheme, readTheme, writeTheme, type ThemeChoice } from "@/lib/theme";

export type SettingsPage = "appearance" | "advanced";

const themes: { id: ThemeChoice; title: string; description: string }[] = [
  { id: "light", title: "Light", description: "Always use the light palette." },
  { id: "dark", title: "Dark", description: "Always use the dark palette." },
  { id: "system", title: "System", description: "Follow this device." },
];

export function SettingsDialog({
  open,
  page,
  savedToken,
  onOpenChange,
  onPageChange,
  onSaveKey,
}: {
  open: boolean;
  page: SettingsPage;
  savedToken: string;
  onOpenChange: (open: boolean) => void;
  onPageChange: (page: SettingsPage) => void;
  onSaveKey: (token: string) => Promise<void>;
}) {
  const [draftTheme, setDraftTheme] = useState<ThemeChoice>(readTheme);
  const [draftKey, setDraftKey] = useState(savedToken);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const wasOpen = useRef(false);

  useEffect(() => {
    if (open && !wasOpen.current) {
      setDraftTheme(readTheme());
      setDraftKey(savedToken);
      setError("");
      setBusy(false);
    }
    if (!open && wasOpen.current) applyTheme(readTheme());
    wasOpen.current = open;
  }, [open, savedToken]);

  useEffect(() => {
    if (open) applyTheme(draftTheme);
  }, [open, draftTheme]);

  async function save() {
    writeTheme(draftTheme);
    const next = draftKey.trim();
    if (next === savedToken.trim()) {
      onOpenChange(false);
      return;
    }
    setBusy(true);
    setError("");
    try {
      await onSaveKey(next);
      onOpenChange(false);
    } catch (err) {
      if (!isAbort(err)) setError(message(err));
    } finally {
      setBusy(false);
    }
  }

  const pageName = page === "appearance" ? "Appearance" : "Advanced";

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="!flex h-[min(34rem,calc(100dvh-2rem))] w-full max-w-[calc(100%-2rem)] flex-col gap-0 overflow-hidden p-0 sm:!max-w-[760px]">
        <DialogHeader className="sr-only">
          <DialogTitle>Settings</DialogTitle>
          <DialogDescription>Choose appearance and the workspace API key.</DialogDescription>
        </DialogHeader>
        <SidebarProvider
          className="!min-h-0 w-full flex-1 flex-col md:flex-row"
          style={{ "--sidebar-width": "14rem" } as CSSProperties}
        >
          <Sidebar
            collapsible="none"
            className="!h-auto !w-full border-b border-sidebar-border md:!h-full md:!w-(--sidebar-width) md:border-r md:border-b-0"
          >
            <SidebarHeader className="hidden md:flex">
              <span className="px-2 text-sm font-medium">Settings</span>
            </SidebarHeader>
            <SidebarContent>
              <SidebarGroup>
                <SidebarGroupContent>
                  <SidebarMenu className="flex-row md:flex-col">
                    <SidebarMenuItem>
                      <SidebarMenuButton
                        isActive={page === "appearance"}
                        onClick={() => onPageChange("appearance")}
                      >
                        <Sun />
                        <span>Appearance</span>
                      </SidebarMenuButton>
                    </SidebarMenuItem>
                    <SidebarMenuItem>
                      <SidebarMenuButton
                        isActive={page === "advanced"}
                        onClick={() => onPageChange("advanced")}
                      >
                        <SlidersHorizontal />
                        <span>Advanced</span>
                      </SidebarMenuButton>
                    </SidebarMenuItem>
                  </SidebarMenu>
                </SidebarGroupContent>
              </SidebarGroup>
            </SidebarContent>
          </Sidebar>
          <SidebarInset className="min-h-0 bg-transparent">
            <header className="flex h-12 shrink-0 items-center px-4 pr-12">
              <Breadcrumb>
                <BreadcrumbList>
                  <BreadcrumbItem>
                    <BreadcrumbPage className="text-muted-foreground">Settings</BreadcrumbPage>
                  </BreadcrumbItem>
                  <BreadcrumbSeparator />
                  <BreadcrumbItem>
                    <BreadcrumbPage>{pageName}</BreadcrumbPage>
                  </BreadcrumbItem>
                </BreadcrumbList>
              </Breadcrumb>
            </header>
            <div className="min-h-0 flex-1 overflow-auto px-4 pb-4">
              {page === "appearance" ? (
                <div className="space-y-1">
                  {themes.map((theme) => (
                    <div
                      key={theme.id}
                      className="flex items-center justify-between gap-4 border-b border-border py-4"
                    >
                      <div>
                        <Label htmlFor={`theme-${theme.id}`}>{theme.title}</Label>
                        <p className="mt-1 text-xs text-muted-foreground">{theme.description}</p>
                      </div>
                      <Switch
                        id={`theme-${theme.id}`}
                        checked={draftTheme === theme.id}
                        onCheckedChange={(checked) => {
                          if (checked) setDraftTheme(theme.id);
                        }}
                      />
                    </div>
                  ))}
                </div>
              ) : (
                <div className="grid max-w-md gap-2">
                  <Label htmlFor="workspace-key">Workspace API key</Label>
                  <Input
                    id="workspace-key"
                    type="password"
                    autoComplete="off"
                    spellCheck={false}
                    placeholder="Workspace API key"
                    value={draftKey}
                    onChange={(event) => setDraftKey(event.target.value)}
                  />
                  <p className="text-xs text-muted-foreground">
                    Stored in this browser and sent as a bearer token. Clear the field and save to
                    disconnect. The server address is fixed when this console is built.
                  </p>
                  {error && (
                    <p className="text-sm text-destructive" role="alert">
                      {error}
                    </p>
                  )}
                </div>
              )}
            </div>
            <div className="flex shrink-0 justify-end border-t border-border px-4 py-3">
              <Button type="button" onClick={() => void save()} disabled={busy}>
                {busy ? "Saving…" : "Save"}
              </Button>
            </div>
          </SidebarInset>
        </SidebarProvider>
      </DialogContent>
    </Dialog>
  );
}
