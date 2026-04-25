"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { Button } from "@/components/ui/button";
import { ApiError } from "@/lib/api/client";
import {
  deleteInstance,
  getInstance,
  type Instance,
} from "@/lib/api/instances";
import { StateBadge } from "./state-badge";

const POLL_INTERVAL_MS = 5_000;

type Props = {
  instanceID: string;
  // sshHost is the public hostname users connect through (proxy.qlaud.net or
  // similar). The subdomain prefix is derived from instanceID at render
  // time.
  sshHost: string;
  // sshUsername is the default cloud-init user — Phase 1 hard-codes this so
  // users see a copy-pasteable command. Phase 8.3 may make it configurable.
  sshUsername: string;
};

export function InstanceDetail({ instanceID, sshHost, sshUsername }: Props) {
  const router = useRouter();
  const [instance, setInstance] = useState<Instance | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);

  useEffect(() => {
    let cancelled = false;
    const controller = new AbortController();

    async function refresh() {
      try {
        const inst = await getInstance(instanceID, controller.signal);
        if (!cancelled) {
          setInstance(inst);
          setError(null);
        }
      } catch (err) {
        if (cancelled || (err instanceof DOMException && err.name === "AbortError")) return;
        setError(err instanceof ApiError ? err.message : "조회에 실패했습니다");
      }
    }

    void refresh();
    const handle = setInterval(refresh, POLL_INTERVAL_MS);
    return () => {
      cancelled = true;
      controller.abort();
      clearInterval(handle);
    };
  }, [instanceID]);

  if (error && !instance) {
    return (
      <p
        role="alert"
        className="rounded-md bg-red-50 px-3 py-2 text-sm text-red-700 dark:bg-red-950/30 dark:text-red-400"
      >
        {error}
      </p>
    );
  }

  if (!instance) {
    return <p className="text-sm text-zinc-500 dark:text-zinc-400">불러오는 중…</p>;
  }

  const subdomain = instance.id.slice(0, 8);
  const sshHostname = `${subdomain}.${sshHost}`;
  const sshCommand = `ssh ${sshUsername}@${sshHostname}`;

  async function handleDelete() {
    if (!confirm(`인스턴스 "${instance!.name}"을(를) 삭제하시겠습니까?`)) return;
    setDeleting(true);
    try {
      await deleteInstance(instance!.id);
      router.push("/instances");
      router.refresh();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "삭제에 실패했습니다");
      setDeleting(false);
    }
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-3">
          <h1 className="text-2xl font-semibold tracking-tight">{instance.name}</h1>
          <StateBadge state={instance.state} />
        </div>
        <Button variant="danger" onClick={handleDelete} disabled={deleting}>
          {deleting ? "삭제 중…" : "삭제"}
        </Button>
      </div>

      <dl className="grid grid-cols-2 gap-4 rounded-lg border border-zinc-200 bg-white p-6 sm:grid-cols-4 dark:border-zinc-800 dark:bg-zinc-900">
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400">
            GPU
          </dt>
          <dd className="mt-1 text-sm font-medium">{instance.gpu_count}</dd>
        </div>
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400">
            vCPU
          </dt>
          <dd className="mt-1 text-sm font-medium">{instance.vcpus}</dd>
        </div>
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400">
            메모리
          </dt>
          <dd className="mt-1 text-sm font-medium">{instance.memory_mb} MiB</dd>
        </div>
        <div>
          <dt className="text-xs font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400">
            내부 IP
          </dt>
          <dd className="mt-1 text-sm font-medium">{instance.vm_internal_ip || "—"}</dd>
        </div>
      </dl>

      {instance.state === "running" && (
        <SSHCommandCard command={sshCommand} hostname={sshHostname} />
      )}

      {instance.state === "failed" && instance.error_message && (
        <p
          role="alert"
          className="rounded-md bg-red-50 px-3 py-2 text-sm text-red-700 dark:bg-red-950/30 dark:text-red-400"
        >
          {instance.error_message}
        </p>
      )}
    </div>
  );
}

function SSHCommandCard({ command, hostname }: { command: string; hostname: string }) {
  const [copied, setCopied] = useState(false);

  async function copy() {
    try {
      await navigator.clipboard.writeText(command);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // Clipboard may be denied (insecure context, etc.); fall through silently.
    }
  }

  return (
    <div className="space-y-2 rounded-lg border border-zinc-200 bg-white p-6 dark:border-zinc-800 dark:bg-zinc-900">
      <h2 className="text-sm font-medium text-zinc-900 dark:text-zinc-100">SSH 접속</h2>
      <p className="text-xs text-zinc-500 dark:text-zinc-400">
        호스트네임 <code className="font-mono">{hostname}</code>
      </p>
      <div className="flex items-center gap-2">
        <code className="flex-1 select-all rounded-md bg-zinc-100 px-3 py-2 font-mono text-sm dark:bg-zinc-800">
          {command}
        </code>
        <Button variant="secondary" type="button" onClick={copy}>
          {copied ? "복사됨" : "복사"}
        </Button>
      </div>
    </div>
  );
}
