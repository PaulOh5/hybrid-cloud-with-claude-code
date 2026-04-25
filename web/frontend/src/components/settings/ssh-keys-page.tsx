"use client";

import { useEffect, useState, type FormEvent } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ApiError } from "@/lib/api/client";
import {
  addSSHKey,
  addSSHKeySchema,
  deleteSSHKey,
  listSSHKeys,
  type SSHKey,
} from "@/lib/api/ssh-keys";

export function SSHKeysPage() {
  const [keys, setKeys] = useState<SSHKey[] | null>(null);
  const [label, setLabel] = useState("");
  const [pubkey, setPubkey] = useState("");
  const [busy, setBusy] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});

  const [refreshTick, setRefreshTick] = useState(0);

  useEffect(() => {
    let cancelled = false;
    const controller = new AbortController();
    listSSHKeys(controller.signal)
      .then((rows) => {
        if (!cancelled) setKeys(rows);
      })
      .catch((err) => {
        if (cancelled || (err instanceof DOMException && err.name === "AbortError")) return;
        setFormError(err instanceof ApiError ? err.message : "키 목록을 불러오지 못했습니다");
      });
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [refreshTick]);

  function refresh() {
    setRefreshTick((n) => n + 1);
  }

  async function handleAdd(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    setFormError(null);
    setFieldErrors({});
    const parsed = addSSHKeySchema.safeParse({ label, pubkey });
    if (!parsed.success) {
      const errs: Record<string, string> = {};
      for (const issue of parsed.error.issues) {
        const k = issue.path[0];
        if (typeof k === "string") errs[k] = issue.message;
      }
      setFieldErrors(errs);
      return;
    }
    setBusy(true);
    try {
      await addSSHKey(parsed.data);
      setLabel("");
      setPubkey("");
      refresh();
    } catch (err) {
      setFormError(err instanceof ApiError ? err.message : "추가에 실패했습니다");
    } finally {
      setBusy(false);
    }
  }

  async function handleDelete(id: string) {
    if (!confirm("이 키를 삭제하시겠습니까?")) return;
    try {
      await deleteSSHKey(id);
      refresh();
    } catch (err) {
      setFormError(err instanceof ApiError ? err.message : "삭제에 실패했습니다");
    }
  }

  return (
    <section className="space-y-8">
      <header className="space-y-1">
        <h1 className="text-2xl font-semibold tracking-tight">SSH 키</h1>
        <p className="text-sm text-zinc-600 dark:text-zinc-400">
          여기에 등록한 키는 새 인스턴스를 만들 때 자동으로 cloud-init에 주입됩니다.
        </p>
      </header>

      <form
        onSubmit={handleAdd}
        className="max-w-2xl space-y-4 rounded-lg border border-zinc-200 bg-white p-6 dark:border-zinc-800 dark:bg-zinc-900"
        noValidate
      >
        <div className="space-y-1.5">
          <Label htmlFor="label">라벨</Label>
          <Input
            id="label"
            value={label}
            onChange={(e) => setLabel(e.target.value)}
            placeholder="laptop"
          />
          {fieldErrors.label && (
            <p role="alert" className="text-sm text-red-600 dark:text-red-400">
              {fieldErrors.label}
            </p>
          )}
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="pubkey">공개키</Label>
          <textarea
            id="pubkey"
            rows={4}
            value={pubkey}
            onChange={(e) => setPubkey(e.target.value)}
            placeholder="ssh-ed25519 AAAA..."
            className="block w-full rounded-md border border-zinc-300 bg-white px-3 py-2 font-mono text-sm dark:border-zinc-700 dark:bg-zinc-900"
          />
          {fieldErrors.pubkey && (
            <p role="alert" className="text-sm text-red-600 dark:text-red-400">
              {fieldErrors.pubkey}
            </p>
          )}
        </div>
        {formError && (
          <p role="alert" className="rounded-md bg-red-50 px-3 py-2 text-sm text-red-700 dark:bg-red-950/30 dark:text-red-400">
            {formError}
          </p>
        )}
        <Button type="submit" disabled={busy}>
          {busy ? "추가 중…" : "추가"}
        </Button>
      </form>

      <div className="space-y-3">
        <h2 className="text-sm font-semibold text-zinc-800 dark:text-zinc-200">등록된 키</h2>
        {keys == null ? (
          <p className="text-sm text-zinc-500 dark:text-zinc-400">불러오는 중…</p>
        ) : keys.length === 0 ? (
          <p className="text-sm text-zinc-500 dark:text-zinc-400">아직 등록된 키가 없습니다.</p>
        ) : (
          <ul className="divide-y divide-zinc-200 rounded-lg border border-zinc-200 bg-white dark:divide-zinc-800 dark:border-zinc-800 dark:bg-zinc-900">
            {keys.map((k) => (
              <li key={k.id} className="flex items-center justify-between gap-4 px-4 py-3">
                <div className="min-w-0">
                  <p className="font-medium text-zinc-900 dark:text-zinc-100">{k.label}</p>
                  <p className="truncate font-mono text-xs text-zinc-500 dark:text-zinc-400">
                    SHA256:{k.fingerprint}
                  </p>
                </div>
                <Button variant="danger" onClick={() => handleDelete(k.id)}>
                  삭제
                </Button>
              </li>
            ))}
          </ul>
        )}
      </div>
    </section>
  );
}
