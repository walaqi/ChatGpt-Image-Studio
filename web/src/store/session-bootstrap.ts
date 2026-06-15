"use client";

import {
  exchangeTicketForSession,
  readEntryTicket,
  stripEntryTicketFromUrl,
} from "@/store/auth";
import { clearLocalImageConversationCache } from "@/store/image-conversations";

// Session bootstrap (docs/multi-tenant-redesign.md §4.6): the mother system
// navigates the iframe to /image-studio/?ticket=<one-time JWT>. On mount the app
// must exchange that ticket for an HttpOnly session cookie BEFORE making any API
// call, then erase the ticket from the URL so it can't leak or be replayed.
//
// A fresh ticket is also the only user-switch signal we get: when a different
// user opens the workbench in the same browser, the mother system hands over a
// new ticket. So presence of a ticket means "(re)establish session for whoever
// this is" — we clear any prior user's browser-side history cache first, closing
// the same-browser cross-user leak (§8 item 4).

export type BootstrapStatus =
  | "no-ticket" // arrived without a ticket — rely on an existing cookie
  | "established" // ticket exchanged, session cookie set
  | "failed"; // a ticket was present but exchange was rejected

export type BootstrapDeps = {
  readTicket: () => string;
  clearLocalCache: () => Promise<void>;
  exchange: (ticket: string) => Promise<boolean>;
  stripTicket: () => void;
};

// runSessionBootstrap is the pure, dependency-injected core so it can be unit
// tested without a DOM. The exported bootstrapSession() wires the real deps.
export async function runSessionBootstrap(
  deps: BootstrapDeps,
): Promise<BootstrapStatus> {
  const ticket = deps.readTicket();
  if (!ticket) {
    return "no-ticket";
  }

  // A ticket means a (possibly different) user is entering. Drop any prior
  // user's local history before establishing the new session, then strip the
  // ticket from the URL regardless of exchange outcome so it can't be replayed.
  await deps.clearLocalCache();
  try {
    const ok = await deps.exchange(ticket);
    return ok ? "established" : "failed";
  } finally {
    deps.stripTicket();
  }
}

export async function bootstrapSession(): Promise<BootstrapStatus> {
  return runSessionBootstrap({
    readTicket: readEntryTicket,
    clearLocalCache: clearLocalImageConversationCache,
    exchange: exchangeTicketForSession,
    stripTicket: stripEntryTicketFromUrl,
  });
}
