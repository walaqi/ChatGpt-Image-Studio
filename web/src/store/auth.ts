"use client";

import webConfig from "@/constants/common-env";

// Multi-tenant auth model (see docs/multi-tenant-redesign.md §4.6):
// the mother system hands image-studio a one-time entry ticket (RS256 JWT) via
// the `?ticket=` query param. The frontend exchanges it for an HttpOnly session
// cookie at POST /auth/session, then never holds a token again — the cookie
// rides along with every same-origin request automatically.
//
// The legacy single-tenant bearer-key flow is gone; the backend still accepts
// it for dev/compat, but image-studio always uses cookies.
//
// Deployment shape: image-studio runs as a SAME-ORIGIN, FULL-PAGE app reverse-
// proxied under the mother system's /image-studio/* prefix (NOT a cross-origin
// iframe). The mother system enters via a full-page navigation, so window.top
// is image-studio itself — there is no parent window to postMessage. The
// requestReauth / requestCreateCredential / requestReturnToConsole helpers below
// therefore navigate the whole page to mother-system routes at the site ROOT
// (outside the /image-studio prefix), using absolute root paths.

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

// requestReauth re-establishes the session after it expires (a 401). image-
// studio cannot mint its own entry ticket — only the mother system can — so it
// navigates the whole page to the mother system's image-studio transit route.
// That Vue route mints a fresh one-time ticket and redirects back to
// /image-studio/?ticket=<jwt>, closing the re-authorization loop.
//
// This is a SAME-ORIGIN FULL-PAGE app (NOT an iframe embed): window.top is
// image-studio itself, so there is no parent window to postMessage. The target
// is a mother-system route at the site ROOT, OUTSIDE the /image-studio/* reverse
// -proxy prefix, so it must be an absolute root path — never built from
// webConfig.apiUrl (which carries the /image-studio prefix).
const REAUTH_PATH = "/image-studio";

export function requestReauth(): void {
  if (typeof window === "undefined") {
    return;
  }
  window.location.assign(REAUTH_PATH);
}

// CONSOLE_PATH is the mother system's console/dashboard, served at the site root
// (image-studio is reverse-proxied under /image-studio/* on the same origin).
const CONSOLE_PATH = "/dashboard";

// requestReturnToConsole navigates the whole page back to the mother system's
// console. The mother system is a same-origin full-page app (NOT an iframe
// embed), so this is a plain top-level navigation rather than a postMessage.
export function requestReturnToConsole(): void {
  if (typeof window === "undefined") {
    return;
  }
  window.location.assign(CONSOLE_PATH);
}

// requestCreateCredential takes the user to the mother system's key-creation
// page so they can create an image-capable channel key. Used by the credential
// picker when the user has no usable image key yet
// (docs/multi-tenant-redesign.md §4.6). The mother system owns the create-key
// UI; image-studio only navigates there.
//
// Same as requestReauth: full-page navigation (no iframe parent), and the target
// is a mother-system route at the site ROOT, outside the /image-studio/* prefix,
// so it must be an absolute root path — never built from webConfig.apiUrl.
const CREATE_KEY_PATH = "/keys";

export function requestCreateCredential(_imageGroupId: number | null): void {
  if (typeof window === "undefined") {
    return;
  }
  window.location.assign(CREATE_KEY_PATH);
}
