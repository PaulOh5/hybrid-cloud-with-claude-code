"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";
import { logout } from "@/lib/api/auth";
import { Button } from "@/components/ui/button";

export function LogoutButton() {
  const router = useRouter();
  const [busy, setBusy] = useState(false);

  return (
    <Button
      variant="ghost"
      disabled={busy}
      onClick={async () => {
        setBusy(true);
        try {
          await logout();
        } catch {
          // Logout is best-effort; ignore network errors and still redirect.
        }
        router.push("/login");
        router.refresh();
      }}
    >
      {busy ? "…" : "로그아웃"}
    </Button>
  );
}
