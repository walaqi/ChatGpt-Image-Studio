import "./globals.css";
import { useEffect, useState } from "react";

import { TopNav } from "@/components/top-nav";
import { ThemeProvider } from "@/components/theme-provider";
import { bootstrapSession } from "@/store/session-bootstrap";

export default function AppShell({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  // Exchange the mother system's one-time entry ticket for a session cookie
  // before rendering anything that talks to the API. When no ticket is present
  // we fall through immediately and rely on an existing cookie (or the 401 →
  // reauth flow). See store/session-bootstrap.ts.
  const [bootstrapped, setBootstrapped] = useState(false);
  useEffect(() => {
    let cancelled = false;
    bootstrapSession().finally(() => {
      if (!cancelled) {
        setBootstrapped(true);
      }
    });
    return () => {
      cancelled = true;
    };
  }, []);

  if (!bootstrapped) {
    return null;
  }

  return (
    <ThemeProvider>
      <main
        className="box-border min-h-screen overflow-y-auto bg-[#f5f5f3] p-3 text-stone-900 transition-colors duration-200 dark:bg-[var(--studio-bg)] dark:text-[var(--studio-text-strong)] lg:h-full lg:min-h-0 lg:overflow-hidden lg:p-4"
        style={{
          fontFamily:
            '"SF Pro Display","SF Pro Text","PingFang SC","Microsoft YaHei","Helvetica Neue",sans-serif',
        }}
      >
        <div className="mx-auto flex min-h-full max-w-[1680px] flex-col gap-3 lg:h-full lg:min-h-0 lg:flex-row lg:gap-4">
          <TopNav />
          <div className="min-w-0 flex-1 lg:min-h-0">{children}</div>
        </div>
      </main>
    </ThemeProvider>
  );
}
