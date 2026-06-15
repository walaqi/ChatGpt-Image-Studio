"use client";

import { Button } from "@/components/ui/button";
import { requestReauth } from "@/store/auth";

// Single-tenant password login is removed in the multi-tenant model (§4.6).
// Sessions bootstrap from the mother system's one-time entry ticket (AppShell).
// This page only shows if someone hits /login directly or the session could not
// be established; it offers a button to request a fresh ticket.
export default function LoginPage() {
  return (
    <div className="flex min-h-[60vh] flex-col items-center justify-center gap-4 px-6 text-center">
      <h1 className="text-xl font-semibold">需要重新登录</h1>
      <p className="max-w-md text-sm text-muted-foreground">
        当前会话已失效或尚未建立。请通过母系统重新进入图片工作台，或点击下方按钮请求重新授权。
      </p>
      <Button onClick={() => requestReauth()}>请求重新授权</Button>
    </div>
  );
}
