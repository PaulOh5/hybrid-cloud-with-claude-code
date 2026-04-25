"use client";

import { useEffect, useRef, useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { DataTable } from "@/components/admin/data-table";
import {
  adminListUsers,
  adminRecharge,
  adminRechargeSchema,
  type AdminUser,
} from "@/lib/api/admin";
import { ApiError } from "@/lib/api/client";

export default function AdminUsersPage() {
  const [rows, setRows] = useState<AdminUser[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [tick, setTick] = useState(0);
  const [target, setTarget] = useState<AdminUser | null>(null);

  useEffect(() => {
    let cancelled = false;
    const ac = new AbortController();
    adminListUsers(ac.signal)
      .then((r) => {
        if (!cancelled) setRows(r);
      })
      .catch((err) => {
        if (cancelled || (err instanceof DOMException && err.name === "AbortError")) return;
        setError(err instanceof ApiError ? err.message : "사용자를 불러오지 못했습니다");
      });
    return () => {
      cancelled = true;
      ac.abort();
    };
  }, [tick]);

  return (
    <section className="space-y-4">
      <h1 className="text-2xl font-semibold tracking-tight">사용자</h1>
      {error && (
        <p role="alert" className="rounded-md bg-red-50 px-3 py-2 text-sm text-red-700 dark:bg-red-950/30 dark:text-red-400">
          {error}
        </p>
      )}
      {rows == null ? (
        <p className="text-sm text-zinc-500">불러오는 중…</p>
      ) : (
        <DataTable
          rows={rows}
          rowKey={(u) => u.id}
          columns={[
            { header: "이메일", cell: (u) => u.email + (u.is_admin ? " (admin)" : "") },
            { header: "잔액 (milli)", cell: (u) => u.balance_milli.toLocaleString(), align: "right" },
            { header: "활성 인스턴스", cell: (u) => u.active_instance_count, align: "right" },
            {
              header: "동작",
              cell: (u) => (
                <Button variant="secondary" onClick={() => setTarget(u)}>
                  크레딧
                </Button>
              ),
              align: "right",
            },
          ]}
        />
      )}
      {target && (
        <RechargePanel
          user={target}
          onDone={() => {
            setTarget(null);
            setTick((n) => n + 1);
          }}
        />
      )}
    </section>
  );
}

function RechargePanel({ user, onDone }: { user: AdminUser; onDone: () => void }) {
  const [delta, setDelta] = useState("100000");
  const [reason, setReason] = useState("manual_topup");
  // Idempotency key seed lives in a ref so render stays pure (eslint
  // react-hooks/purity blocks Date.now() during render). Effect copies into
  // state once, then user can override.
  const seedRef = useRef<string | null>(null);
  const [key, setKey] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    if (seedRef.current == null) {
      seedRef.current = `topup-${Date.now()}`;
      setKey(seedRef.current);
    }
  }, []);

  async function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    setErr(null);
    const parsed = adminRechargeSchema.safeParse({
      delta_milli: Number(delta),
      reason,
      idempotency_key: key,
    });
    if (!parsed.success) {
      setErr(parsed.error.issues.map((i) => i.message).join(", "));
      return;
    }
    setBusy(true);
    try {
      await adminRecharge(user.id, parsed.data);
      onDone();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "충전 실패");
    } finally {
      setBusy(false);
    }
  }

  return (
    <form
      onSubmit={handleSubmit}
      className="max-w-xl space-y-3 rounded-lg border border-zinc-200 bg-white p-6 dark:border-zinc-800 dark:bg-zinc-900"
    >
      <h2 className="text-sm font-semibold">크레딧 충전 — {user.email}</h2>
      <div className="grid grid-cols-2 gap-3">
        <div className="space-y-1.5">
          <Label htmlFor="delta">delta (milli, 음수=차감)</Label>
          <Input
            id="delta"
            type="number"
            value={delta}
            onChange={(e) => setDelta(e.target.value)}
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="reason">사유</Label>
          <Input id="reason" value={reason} onChange={(e) => setReason(e.target.value)} />
        </div>
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="key">idempotency_key</Label>
        <Input id="key" value={key} onChange={(e) => setKey(e.target.value)} />
      </div>
      {err && (
        <p role="alert" className="rounded-md bg-red-50 px-3 py-2 text-sm text-red-700 dark:bg-red-950/30 dark:text-red-400">
          {err}
        </p>
      )}
      <div className="flex gap-2">
        <Button type="submit" disabled={busy}>
          {busy ? "처리 중…" : "충전"}
        </Button>
        <Button type="button" variant="secondary" onClick={onDone} disabled={busy}>
          취소
        </Button>
      </div>
    </form>
  );
}
