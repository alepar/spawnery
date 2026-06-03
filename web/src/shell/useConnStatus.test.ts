import { renderHook, act } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { useConnStatus } from "./useConnStatus";

describe("useConnStatus", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("starts null (indicator hidden)", () => {
    const { result } = renderHook(() => useConnStatus(5000));
    expect(result.current.conn).toBe(null);
  });

  it("connecting -> slow after the timeout", () => {
    const { result } = renderHook(() => useConnStatus(5000));
    act(() => result.current.connecting());
    expect(result.current.conn).toBe("connecting");
    act(() => { vi.advanceTimersByTime(5000); });
    expect(result.current.conn).toBe("slow");
  });

  it("connected before the timeout stays connected (timer cleared)", () => {
    const { result } = renderHook(() => useConnStatus(5000));
    act(() => result.current.connecting());
    act(() => result.current.connected());
    expect(result.current.conn).toBe("connected");
    act(() => { vi.advanceTimersByTime(5000); });
    expect(result.current.conn).toBe("connected");
  });

  it("errored sets error; closed keeps error; reset clears it", () => {
    const { result } = renderHook(() => useConnStatus(5000));
    act(() => result.current.errored());
    expect(result.current.conn).toBe("error");
    act(() => result.current.closed());
    expect(result.current.conn).toBe("error"); // error preserved across an unexpected close
    act(() => result.current.reset());
    expect(result.current.conn).toBe(null);
  });

  it("closed while connecting hides (null)", () => {
    const { result } = renderHook(() => useConnStatus(5000));
    act(() => result.current.connecting());
    act(() => result.current.closed());
    expect(result.current.conn).toBe(null);
  });

  it("waiting sets the waiting state and clears any slow timer", () => {
    const { result } = renderHook(() => useConnStatus(5000));
    act(() => result.current.connecting());
    act(() => result.current.waiting());
    expect(result.current.conn).toBe("waiting");
    act(() => { vi.advanceTimersByTime(5000); });
    expect(result.current.conn).toBe("waiting");
  });
});

describe("useConnStatus reconnecting", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("reconnecting -> disconnected after the grace window", () => {
    const { result } = renderHook(() => useConnStatus(5000, 12000));
    act(() => result.current.reconnecting());
    expect(result.current.conn).toBe("reconnecting");
    act(() => vi.advanceTimersByTime(12000));
    expect(result.current.conn).toBe("disconnected");
  });

  it("connected() during the grace window cancels the -> disconnected transition", () => {
    const { result } = renderHook(() => useConnStatus(5000, 12000));
    act(() => result.current.reconnecting());
    act(() => result.current.connected());
    act(() => vi.advanceTimersByTime(12000));
    expect(result.current.conn).toBe("connected");
  });

  it("reconnecting() while connecting cancels the -> slow transition", () => {
    const { result } = renderHook(() => useConnStatus(5000, 12000));
    act(() => result.current.connecting());
    act(() => result.current.reconnecting());
    expect(result.current.conn).toBe("reconnecting");
    act(() => vi.advanceTimersByTime(5000));
    expect(result.current.conn).toBe("reconnecting"); // the slow timer was cleared
  });
});
