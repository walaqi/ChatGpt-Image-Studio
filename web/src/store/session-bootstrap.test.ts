import { describe, expect, it, vi } from "vitest";

import { runSessionBootstrap, type BootstrapDeps } from "./session-bootstrap";

function makeDeps(overrides: Partial<BootstrapDeps> = {}): {
  deps: BootstrapDeps;
  calls: {
    clearLocalCache: ReturnType<typeof vi.fn>;
    exchange: ReturnType<typeof vi.fn>;
    stripTicket: ReturnType<typeof vi.fn>;
  };
} {
  const clearLocalCache = vi.fn(async () => {});
  const exchange = vi.fn(async () => true);
  const stripTicket = vi.fn();
  const deps: BootstrapDeps = {
    readTicket: () => "ticket-abc",
    clearLocalCache,
    exchange,
    stripTicket,
    ...overrides,
  };
  return { deps, calls: { clearLocalCache, exchange, stripTicket } };
}

describe("runSessionBootstrap", () => {
  it("does nothing but report no-ticket when no ticket is present", async () => {
    const { deps, calls } = makeDeps({ readTicket: () => "" });
    const status = await runSessionBootstrap(deps);
    expect(status).toBe("no-ticket");
    expect(calls.clearLocalCache).not.toHaveBeenCalled();
    expect(calls.exchange).not.toHaveBeenCalled();
    expect(calls.stripTicket).not.toHaveBeenCalled();
  });

  it("clears local cache BEFORE exchanging, then strips the ticket", async () => {
    const order: string[] = [];
    const { deps } = makeDeps({
      clearLocalCache: vi.fn(async () => {
        order.push("clear");
      }),
      exchange: vi.fn(async () => {
        order.push("exchange");
        return true;
      }),
      stripTicket: vi.fn(() => {
        order.push("strip");
      }),
    });
    const status = await runSessionBootstrap(deps);
    expect(status).toBe("established");
    // Cache must be wiped before the new session is established (so a prior
    // user's history can never be read), and the ticket erased afterwards.
    expect(order).toEqual(["clear", "exchange", "strip"]);
  });

  it("reports failed but still strips the ticket when exchange is rejected", async () => {
    const { deps, calls } = makeDeps({ exchange: vi.fn(async () => false) });
    const status = await runSessionBootstrap(deps);
    expect(status).toBe("failed");
    expect(calls.clearLocalCache).toHaveBeenCalledOnce();
    expect(calls.stripTicket).toHaveBeenCalledOnce();
  });

  it("strips the ticket even when exchange throws (no replayable ticket left)", async () => {
    const { deps, calls } = makeDeps({
      exchange: vi.fn(async () => {
        throw new Error("network down");
      }),
    });
    await expect(runSessionBootstrap(deps)).rejects.toThrow("network down");
    // The finally block must still erase the ticket so it can't be replayed.
    expect(calls.stripTicket).toHaveBeenCalledOnce();
    expect(calls.clearLocalCache).toHaveBeenCalledOnce();
  });
});
