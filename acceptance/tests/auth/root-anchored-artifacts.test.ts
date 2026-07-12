import { execFileSync } from "node:child_process";
import { describe, expect, it } from "vitest";
import { posixShellQuote } from "./root-anchored-artifacts";

describe("posixShellQuote", () => {
  it.each([
    "",
    "acct-owner",
    "owner with spaces",
    "owner'; touch /tmp/not-created; printf '",
    "$(printf injected) `printf again` $HOME & | ; < >",
    "line one\nline two",
  ])("round-trips one literal shell argument", (value) => {
    const output = execFileSync("/bin/sh", ["-c", `printf %s ${posixShellQuote(value)}`]);
    expect(output.toString()).toBe(value);
  });
});
