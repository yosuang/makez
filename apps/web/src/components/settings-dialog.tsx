import { useState } from "react";
import { Button } from "@makez/ui/components/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@makez/ui/components/dialog";
import { applyTheme, readTheme, writeTheme, type ThemeChoice } from "@/lib/theme";
export type SettingsPage = "appearance" | "advanced";
export function SettingsDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const [theme, setTheme] = useState<ThemeChoice>(readTheme);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Settings</DialogTitle>
          <DialogDescription>Choose your appearance.</DialogDescription>
        </DialogHeader>
        <label>
          Theme{" "}
          <select
            aria-label="Theme"
            value={theme}
            onChange={(event) => {
              const value = event.target.value as ThemeChoice;
              setTheme(value);
              writeTheme(value);
              applyTheme(value);
            }}
          >
            <option value="system">System</option>
            <option value="light">Light</option>
            <option value="dark">Dark</option>
          </select>
        </label>
        <Button onClick={() => onOpenChange(false)}>Done</Button>
      </DialogContent>
    </Dialog>
  );
}
