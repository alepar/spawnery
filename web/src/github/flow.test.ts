import { describe, it, expect, beforeEach } from "vitest";
import {
  GH_FLOW_MARKER_KEY, setFlowMarker, getFlowMarker, clearFlowMarker,
  parseLinkError, recoverStaleFlowMarker, linkErrorMessage,
} from "./flow";

beforeEach(() => { sessionStorage.clear(); });

describe("flow marker", () => {
  it("round-trips through sessionStorage", () => {
    expect(getFlowMarker()).toBeNull();
    setFlowMarker("flow-abc");
    expect(sessionStorage.getItem(GH_FLOW_MARKER_KEY)).toBe("flow-abc");
    expect(getFlowMarker()).toBe("flow-abc");
    clearFlowMarker();
    expect(getFlowMarker()).toBeNull();
  });
});

describe("parseLinkError", () => {
  it("extracts ?error= and returns null when absent", () => {
    expect(parseLinkError("?error=access_denied&foo=1")).toBe("access_denied");
    expect(parseLinkError("?foo=1")).toBeNull();
    expect(parseLinkError("")).toBeNull();
  });
});

describe("recoverStaleFlowMarker", () => {
  it("clears the marker on a bootstrap-failure status and reports it", () => {
    setFlowMarker("flow-1");
    expect(recoverStaleFlowMarker("login-required")).toBe(true);
    expect(getFlowMarker()).toBeNull();
  });
  it("is a no-op on authed / loading", () => {
    setFlowMarker("flow-1");
    expect(recoverStaleFlowMarker("authed")).toBe(false);
    expect(recoverStaleFlowMarker("loading")).toBe(false);
    expect(getFlowMarker()).toBe("flow-1");
  });
  it("is a no-op when no marker is present", () => {
    expect(recoverStaleFlowMarker("key-lost")).toBe(false);
  });
});

describe("linkErrorMessage", () => {
  it("maps access_denied and falls back for unknown codes", () => {
    expect(linkErrorMessage("access_denied")).toMatch(/declined/i);
    expect(linkErrorMessage("weird_code")).toMatch(/weird_code/);
  });
});
