"use client";

import { useCallback, useEffect, useState } from "react";
import { KeyRound, Loader2, RefreshCw } from "lucide-react";
import { toast } from "sonner";

import {
  fetchCredentialKeys,
  setCurrentCredential,
  type CredentialKeyCandidate,
  type CredentialKeyListResult,
} from "@/lib/api";
import { requestCreateCredential } from "@/store/auth";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { cn } from "@/lib/utils";

// CredentialPicker drives the per-user channel-key selection required by the
// cpa-only multi-tenant model (docs/multi-tenant-redesign.md §4.2/§4.6). The
// backend resolves the user's plaintext credential from the remembered key_id,
// so without a selection image generation cannot run. The picker lists the
// user's image-capable keys, remembers the chosen one, and guides the user back
// to the mother system when no usable key exists.

function formatExpiry(value: string) {
  const trimmed = String(value || "").trim();
  if (!trimmed) {
    return "长期有效";
  }
  const date = new Date(trimmed);
  if (Number.isNaN(date.getTime())) {
    return trimmed;
  }
  return new Intl.DateTimeFormat("zh-CN", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
  }).format(date);
}

function formatQuota(candidate: CredentialKeyCandidate) {
  const remaining = Math.max(0, candidate.quota - candidate.quota_used);
  return `${remaining} / ${candidate.quota}`;
}

type LoadState =
  | { status: "idle" }
  | { status: "loading" }
  | { status: "ready"; result: CredentialKeyListResult }
  | { status: "error"; message: string };

function renderBody({
  load,
  selectedKeyId,
  savingKeyId,
  onSelect,
  onRetry,
}: {
  load: LoadState;
  selectedKeyId: number | null;
  savingKeyId: number | null;
  onSelect: (keyId: number) => void;
  onRetry: () => void;
}) {
  if (load.status === "loading" || load.status === "idle") {
    return (
      <div className="flex items-center justify-center gap-2 py-10 text-sm text-stone-500">
        <Loader2 className="size-4 animate-spin" />
        正在加载可用的渠道 key…
      </div>
    );
  }

  if (load.status === "error") {
    return (
      <div className="flex flex-col items-center gap-3 py-8 text-center">
        <p className="text-sm text-stone-600">{load.message}</p>
        <Button type="button" variant="outline" className="gap-1.5" onClick={onRetry}>
          <RefreshCw className="size-4" />
          重试
        </Button>
      </div>
    );
  }

  const { result } = load;
  if (result.keys.length === 0) {
    return (
      <div className="flex flex-col items-center gap-3 py-8 text-center">
        {result.can_create ? (
          <>
            <p className="text-sm text-stone-600">
              当前没有可用于出图的渠道 key。请回到母系统，在图片专用分组下创建一个 key 后再回来选择。
            </p>
            <Button
              type="button"
              className="gap-1.5"
              onClick={() => requestCreateCredential(result.image_group_id)}
            >
              <KeyRound className="size-4" />
              去母系统创建 key
            </Button>
          </>
        ) : (
          <p className="text-sm text-stone-600">
            管理员尚未开启图片功能（未配置图片专用分组）。请联系管理员开通后再使用出图。
          </p>
        )}
      </div>
    );
  }

  return (
    <ul className="flex flex-col gap-2">
      {result.keys.map((candidate) => {
        const active = candidate.key_id === selectedKeyId;
        const saving = candidate.key_id === savingKeyId;
        return (
          <li key={candidate.key_id}>
            <button
              type="button"
              disabled={saving}
              onClick={() => onSelect(candidate.key_id)}
              className={cn(
                "flex w-full flex-col gap-1 rounded-2xl border px-4 py-3 text-left transition",
                active
                  ? "border-stone-900 bg-stone-50 dark:border-[var(--studio-accent-strong)] dark:bg-[var(--studio-panel-soft)]"
                  : "border-stone-200 bg-white hover:bg-stone-50 dark:border-[var(--studio-border)] dark:bg-[var(--studio-panel)] dark:hover:bg-[var(--studio-panel-soft)]",
                saving && "opacity-60",
              )}
            >
              <div className="flex items-center justify-between gap-2">
                <span className="truncate text-sm font-medium text-stone-900 dark:text-[var(--studio-text-strong)]">
                  {candidate.name}
                </span>
                {active ? (
                  <span className="shrink-0 rounded-full bg-stone-900 px-2 py-0.5 text-[11px] font-medium text-white dark:bg-[var(--studio-accent-strong)] dark:text-[var(--studio-accent-foreground)]">
                    当前
                  </span>
                ) : saving ? (
                  <Loader2 className="size-4 shrink-0 animate-spin text-stone-500" />
                ) : null}
              </div>
              <div className="flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-stone-500 dark:text-[var(--studio-text-muted)]">
                <span>分组：{candidate.group_name || "—"}</span>
                <span>额度：{formatQuota(candidate)}</span>
                <span>到期：{formatExpiry(candidate.expires_at)}</span>
              </div>
            </button>
          </li>
        );
      })}
    </ul>
  );
}

export function CredentialPicker() {
  const [open, setOpen] = useState(false);
  const [load, setLoad] = useState<LoadState>({ status: "idle" });
  const [selectedKeyId, setSelectedKeyId] = useState<number | null>(null);
  const [savingKeyId, setSavingKeyId] = useState<number | null>(null);

  const refresh = useCallback(async () => {
    setLoad({ status: "loading" });
    try {
      const result = await fetchCredentialKeys();
      setLoad({ status: "ready", result });
      setSelectedKeyId(
        typeof result.current_key_id === "number" ? result.current_key_id : null,
      );
    } catch (error) {
      setLoad({
        status: "error",
        message:
          error instanceof Error ? error.message : "无法加载可用的渠道 key",
      });
    }
  }, []);

  // Load the current selection once on mount so the trigger can show the active
  // key name without opening the dialog.
  useEffect(() => {
    void refresh();
  }, [refresh]);

  const handleSelect = useCallback(
    async (keyId: number) => {
      setSavingKeyId(keyId);
      try {
        await setCurrentCredential(keyId);
        setSelectedKeyId(keyId);
        toast.success("已记住该 key，之后出图将使用它");
        setOpen(false);
      } catch (error) {
        toast.error(
          error instanceof Error ? error.message : "保存选择失败，请重试",
        );
      } finally {
        setSavingKeyId(null);
      }
    },
    [],
  );

  const result = load.status === "ready" ? load.result : null;
  const selectedKey =
    result?.keys.find((item) => item.key_id === selectedKeyId) ?? null;
  const triggerLabel = selectedKey ? selectedKey.name : "选择出图 key";

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (next) {
          void refresh();
        }
      }}
    >
      <DialogTrigger asChild>
        <Button
          type="button"
          variant="outline"
          className={cn(
            "h-8 shrink-0 gap-1.5 rounded-full border-stone-200 bg-white px-3 text-stone-700 shadow-none dark:border-[var(--studio-border)] dark:bg-[var(--studio-panel-soft)] dark:text-[var(--studio-text)] sm:h-9 sm:px-4",
            !selectedKey && "border-amber-300 text-amber-700",
          )}
          title={selectedKey ? `当前出图 key：${selectedKey.name}` : "尚未选择出图 key"}
        >
          <KeyRound className="size-4" />
          <span className="max-w-[140px] truncate">{triggerLabel}</span>
        </Button>
      </DialogTrigger>
      <DialogContent className="max-h-[80vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>选择出图 key</DialogTitle>
          <DialogDescription>
            出图会使用你在母系统的渠道 key。选定后系统会记住，之后无需再次选择。
          </DialogDescription>
        </DialogHeader>
        {renderBody({
          load,
          selectedKeyId,
          savingKeyId,
          onSelect: handleSelect,
          onRetry: () => void refresh(),
        })}
      </DialogContent>
    </Dialog>
  );
}
