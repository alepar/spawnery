import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, beforeEach } from "vitest";
import { SettingsView } from "./SettingsView";

describe("SettingsView", () => {
  beforeEach(() => { document.documentElement.className = ""; localStorage.clear(); });

  it("toggling the switch flips the .dark class and persists", async () => {
    render(<SettingsView />);
    const toggle = screen.getByTestId("theme-toggle");
    expect(document.documentElement.classList.contains("dark")).toBe(false);
    await userEvent.click(toggle);
    expect(document.documentElement.classList.contains("dark")).toBe(true);
    expect(localStorage.getItem("theme")).toBe("dark");
  });
});
