import { httpRequest } from "@/lib/request";
import webConfig from "@/constants/common-env";

export type ImageModel = "gpt-image-1" | "gpt-image-2";
export type ImageQuality = "low" | "medium" | "high";
export type ImageResolutionAccess = "free" | "paid";
export type ImageResponseItem = {
  url?: string;
  b64_json?: string;
  revised_prompt?: string;
  file_id?: string;
  gen_id?: string;
  conversation_id?: string;
  parent_message_id?: string;
  source_account_id?: string;
  error?: string;
};

export type ImageTaskStatus =
  | "queued"
  | "running"
  | "succeeded"
  | "failed"
  | "cancel_requested"
  | "cancelled"
  | "expired";

export type ImageTaskWaitingReason =
  | ""
  | "global_concurrency"
  | "paid_account_busy"
  | "compatible_account_busy"
  | "source_account_busy"
  | "retry_backoff";

export type ImageTaskBlocker = {
  code: string;
  detail?: string;
};

export type ImageTaskView = {
  id: string;
  conversationId: string;
  turnId: string;
  mode: "generate" | "edit" | string;
  status: ImageTaskStatus;
  createdAt: string;
  startedAt?: string;
  finishedAt?: string;
  count: number;
  retryImageIndex?: number;
  queuePosition?: number;
  waitingReason?: ImageTaskWaitingReason;
  blockers?: ImageTaskBlocker[];
  images: ImageResponseItem[];
  error?: string;
  cancelRequested?: boolean;
};

export type ImageTaskSnapshot = {
  running: number;
  maxRunning: number;
  queued: number;
  total: number;
  activeSources: {
    workspace: number;
    compat: number;
  };
  finalStatuses: {
    succeeded: number;
    failed: number;
    cancelled: number;
    expired: number;
  };
  retentionSeconds: number;
};

export type ImageTaskStreamEvent = {
  type: string;
  taskId?: string;
  task?: ImageTaskView;
  snapshot?: ImageTaskSnapshot;
};

export type InpaintSourceReference = {
  original_file_id: string;
  original_gen_id: string;
  conversation_id?: string;
  parent_message_id?: string;
  source_account_id: string;
};

type ImageTaskListResponse = {
  items: ImageTaskView[];
  snapshot: ImageTaskSnapshot;
};

type ImageTaskResponse = {
  task: ImageTaskView;
  snapshot: ImageTaskSnapshot;
};

export type VersionInfo = {
  version: string;
  commit?: string;
  buildTime?: string;
};

// CredentialKeyCandidate is one image-capable key the user can pick (no
// plaintext — only display metadata). Field shapes match the mother system's
// GET /internal/cred/keys response: quota is a number (0 = unlimited),
// expires_at is a Unix-second timestamp or null (never expires).
export type CredentialKeyCandidate = {
  key_id: number;
  name: string;
  quota: number;
  quota_used: number;
  expires_at: number | null;
  group_id: number;
  group_name: string;
};

export type CredentialKeyListResult = {
  keys: CredentialKeyCandidate[];
  can_create: boolean;
  image_group_id: number | null;
  // The backend echoes the currently remembered selection when one exists so
  // the picker can pre-highlight it.
  current_key_id?: number;
};

// exchangeEntryTicket posts a one-time entry ticket to establish the session
// cookie. The cookie is set via Set-Cookie on the response.
export async function exchangeEntryTicket(ticket: string) {
  return httpRequest<{ ok: boolean; version?: string }>("/auth/session", {
    method: "POST",
    headers: { Authorization: `Bearer ${ticket}` },
    redirectOnUnauthorized: false,
  });
}

export async function fetchCredentialKeys() {
  return httpRequest<CredentialKeyListResult>("/api/image/credential/keys");
}

export async function fetchCurrentCredential() {
  return httpRequest<{ selected: boolean; key_id?: number }>(
    "/api/image/credential/current",
  );
}

export async function setCurrentCredential(keyId: number) {
  return httpRequest<{ ok: boolean; key_id: number }>(
    "/api/image/credential/current",
    {
      method: "PUT",
      body: { key_id: keyId },
    },
  );
}

export async function fetchVersionInfo() {
  return httpRequest<VersionInfo>("/version", {
    redirectOnUnauthorized: false,
  });
}

export async function createImageTask(payload: {
  taskId?: string;
  conversationId: string;
  turnId: string;
  mode: "generate" | "edit";
  prompt: string;
  model?: ImageModel;
  count?: number;
  retryImageIndex?: number;
  size?: string;
  resolutionAccess?: ImageResolutionAccess;
  quality?: ImageQuality;
  sourceImages?: Array<{
    id: string;
    role: "image" | "mask";
    name: string;
    dataUrl?: string;
    url?: string;
  }>;
  sourceReference?: InpaintSourceReference;
}) {
  return httpRequest<ImageTaskResponse>("/api/image/tasks", {
    method: "POST",
    body: {
      taskId: payload.taskId?.trim() || undefined,
      conversationId: payload.conversationId,
      turnId: payload.turnId,
      mode: payload.mode,
      prompt: payload.prompt,
      model: payload.model ?? "gpt-image-2",
      count: Math.max(1, payload.count ?? 1),
      retryImageIndex:
        typeof payload.retryImageIndex === "number"
          ? payload.retryImageIndex
          : undefined,
      size: payload.size?.trim() || undefined,
      resolutionAccess: payload.resolutionAccess,
      quality: payload.quality,
      sourceImages: payload.sourceImages ?? [],
      sourceReference: payload.sourceReference,
    },
  });
}

export async function listImageTasks() {
  return httpRequest<ImageTaskListResponse>("/api/image/tasks");
}

export async function cancelImageTask(taskId: string) {
  return httpRequest<ImageTaskResponse>(
    `/api/image/tasks/${encodeURIComponent(taskId)}`,
    {
      method: "DELETE",
    },
  );
}

export async function consumeImageTaskStream(
  handlers: {
    onInit: (payload: { items: ImageTaskView[]; snapshot: ImageTaskSnapshot }) => void;
    onEvent: (event: ImageTaskStreamEvent) => void;
  },
  signal: AbortSignal,
) {
  const response = await fetch(
    `${webConfig.apiUrl.replace(/\/$/, "")}/api/image/tasks/stream`,
    {
      method: "GET",
      headers: { Accept: "text/event-stream" },
      credentials: "include",
      signal,
    },
  );
  if (!response.ok || !response.body) {
    throw new Error(`task stream failed (${response.status})`);
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let eventType = "message";
  let dataLines: string[] = [];

  const flushEvent = () => {
    if (dataLines.length === 0) {
      eventType = "message";
      return;
    }
    const raw = dataLines.join("\n");
    dataLines = [];
    try {
      if (eventType === "init") {
        handlers.onInit(JSON.parse(raw) as { items: ImageTaskView[]; snapshot: ImageTaskSnapshot });
      } else {
        handlers.onEvent(JSON.parse(raw) as ImageTaskStreamEvent);
      }
    } finally {
      eventType = "message";
    }
  };

  while (true) {
    const { value, done } = await reader.read();
    if (done) {
      flushEvent();
      return;
    }
    buffer += decoder.decode(value, { stream: true });
    const lines = buffer.split(/\r?\n/);
    buffer = lines.pop() ?? "";

    for (const line of lines) {
      if (!line) {
        flushEvent();
        continue;
      }
      if (line.startsWith("event:")) {
        eventType = line.slice(6).trim() || "message";
        continue;
      }
      if (line.startsWith("data:")) {
        dataLines.push(line.slice(5).trimStart());
      }
    }
  }
}
