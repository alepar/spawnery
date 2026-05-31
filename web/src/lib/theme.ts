export function setTheme(theme: "light" | "dark") {
  document.documentElement.classList.toggle("dark", theme === "dark");
  try { localStorage.setItem("theme", theme); } catch { /* private mode */ }
}

export function initialTheme(): "light" | "dark" {
  try {
    const s = localStorage.getItem("theme");
    if (s === "dark" || s === "light") return s;
  } catch { /* private mode */ }
  return matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}
