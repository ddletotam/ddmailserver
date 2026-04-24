const THEME_KEY = "ddmail_theme";

type Theme = "light" | "dark";

function loadTheme(): Theme {
  const saved = localStorage.getItem(THEME_KEY);
  if (saved === "dark" || saved === "light") return saved;
  // Follow system preference
  if (window.matchMedia?.("(prefers-color-scheme: dark)").matches) return "dark";
  return "light";
}

function applyTheme(theme: Theme) {
  document.documentElement.classList.toggle("dark", theme === "dark");
}

const initialTheme = loadTheme();
let current = $state<Theme>(initialTheme);
applyTheme(initialTheme);

export const themeStore = {
  get current() {
    return current;
  },
  get isDark() {
    return current === "dark";
  },
  toggle() {
    current = current === "dark" ? "light" : "dark";
    localStorage.setItem(THEME_KEY, current);
    applyTheme(current);
  },
};
