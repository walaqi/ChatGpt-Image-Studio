"use client";

import webConfig from "@/constants/common-env";

// Multi-tenant auth model (see docs/multi-tenant-redesign.md §4.6):
// the mother system hands image-studio a one-time entry ticket (RS256 JWT) via
// the `?ticket=` query param. The frontend exchanges it for an HttpOnly session
// cookie at POST /auth/session, then never holds a token again — the cookie
// rides along with every same-origin request automatically.
//
// The legacy single-tenant bearer-key flow is gone; the backend still accepts
// it for dev/compat, but the embedded frontend always uses cookies.

const TICKET_QUERY_PARAM = "ticket";

// readEntryTicket pulls the one-time ticket out of the current URL (query string
// or hash). Returns "" when none is present.
export function readEntryTicket(): string {
  if (typeof window === "undefined") {
    return "";
  }
  const fromSearch = new URLSearchParams(window.location.search).get(TICKET_QUERY_PARAM);
  if (fromSearch && fromSearch.trim()) {
    return fromSearch.trim();
  }
  // Some embeds pass the ticket in the hash to keep it out of server logs.
  const hash = window.location.hash.replace(/^#/, "");
  if (hash) {
    const fromHash = new URLSearchParams(hash).get(TICKET_QUERY_PARAM);
    if (fromHash && fromHash.trim()) {
      return fromHash.trim();
    }
  }
  return "";
}

// stripEntryTicketFromUrl removes the ticket from the visible URL after exchange
// so it cannot leak via history/back-forward or be replayed.
export function stripEntryTicketFromUrl(): void {
  if (typeof window === "undefined") {
    return;
  }
  const url = new URL(window.location.href);
  let changed = false;
  if (url.searchParams.has(TICKET_QUERY_PARAM)) {
    url.searchParams.delete(TICKET_QUERY_PARAM);
    changed = true;
  }
  if (url.hash) {
    const hashParams = new URLSearchParams(url.hash.replace(/^#/, ""));
    if (hashParams.has(TICKET_QUERY_PARAM)) {
      hashParams.delete(TICKET_QUERY_PARAM);
      const next = hashParams.toString();
      url.hash = next ? `#${next}` : "";
      changed = true;
    }
  }
  if (changed) {
    window.history.replaceState(window.history.state, "", url.toString());
  }
}

// exchangeTicketForSession posts the entry ticket to the backend, which verifies
// it and sets the HttpOnly session cookie. The ticket goes in the Authorization
// header; the cookie comes back via Set-Cookie (handled by the browser).
export async function exchangeTicketForSession(ticket: string): Promise<boolean> {
  const trimmed = String(ticket || "").trim();
  if (!trimmed) {
    return false;
  }
  const base = webConfig.apiUrl.replace(/\/$/, "");
  const response = await fetch(`${base}/auth/session`, {
    method: "POST",
    credentials: "include",
    headers: { Authorization: `Bearer ${trimmed}` },
  });
  return response.ok;
}

// requestReauth asks the parent (mother system) window to re-issue an entry
// ticket. Used when the session cookie has expired (401). When not embedded,
// there is nothing we can do but surface the state to the caller.
export function requestReauth(): void {
  if (typeof window === "undefined") {
    return;
  }
  if (window.parent && window.parent !== window) {
    // The mother system listens for this and re-navigates the iframe with a
    // fresh ?ticket=. Origin is "*" here because the parent validates on its
    // side; we never send sensitive data in this message.
    window.parent.postMessage({ type: "image-studio:reauth" }, "*");
  }
}

// requestCreateCredential asks the parent (mother system) window to take the
// user to its key-creation flow under the preset image group. Used by the
// credential picker when the user has no usable image key yet
// (docs/multi-tenant-redesign.md §4.6). The mother system owns the actual
// create-key UI; image-studio only signals intent and the target group.
export function requestCreateCredential(imageGroupId: number | null): void {
  if (typeof window === "undefined") {
    return;
  }
  if (window.parent && window.parent !== window) {
    window.parent.postMessage(
      { type: "image-studio:create-credential", imageGroupId },
      "*",
    );
  }
}
