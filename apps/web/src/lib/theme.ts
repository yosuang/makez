export type ThemeChoice = "light" | "dark" | "system";

const storageKey = "makez.theme";

export function readTheme(): ThemeChoice {
  try {
    const value = localStorage.getItem(storageKey);
    if (value === "light" || value === "dark" || value === "system") return value;
  } catch {
    // Ignore storage failures and keep the default dark console.
  }
  return "dark";
}

export function applyTheme(theme: ThemeChoice) {
  const dark =
    theme === "dark" ||
    (theme === "system" && window.matchMedia("(prefers-color-scheme: dark)").matches);
  document.documentElement.classList.toggle("dark", dark);
  document.documentElement.style.colorScheme = dark ? "dark" : "light";
}

export function writeTheme(theme: ThemeChoice) {
  try {
    localStorage.setItem(storageKey, theme);
  } catch {
    // The choice still applies for this page.
  }
  applyTheme(theme);
}

export function watchSystemTheme() {
  const media = window.matchMedia("(prefers-color-scheme: dark)");
  const onChange = () => {
    if (readTheme() === "system") applyTheme("system");
  };
  media.addEventListener("change", onChange);
  return () => media.removeEventListener("change", onChange);
}
