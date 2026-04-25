"use client";

import { useEffect, useState, type FormEvent } from "react";
import { useRouter } from "next/navigation";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  createInstance,
  createInstanceSchema,
  type CreateInstanceInput,
} from "@/lib/api/instances";
import { listNodes, type UserNode } from "@/lib/api/nodes";
import { ApiError } from "@/lib/api/client";

const GPU_CHOICES = [0, 1, 2, 4] as const;

const GPU_PRESETS: Record<number, { memory_mb: number; vcpus: number }> = {
  0: { memory_mb: 2048, vcpus: 2 },
  1: { memory_mb: 16384, vcpus: 8 },
  2: { memory_mb: 32768, vcpus: 16 },
  4: { memory_mb: 65536, vcpus: 32 },
};

export function CreateInstanceForm() {
  const router = useRouter();
  const [nodes, setNodes] = useState<UserNode[] | null>(null);
  const [name, setName] = useState("");
  const [nodeID, setNodeID] = useState("");
  const [gpuCount, setGPUCount] = useState<number>(1);
  const [memoryMb, setMemoryMb] = useState<number>(GPU_PRESETS[1].memory_mb);
  const [vcpus, setVCPUs] = useState<number>(GPU_PRESETS[1].vcpus);
  const [sshPubkey, setSSHPubkey] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});

  useEffect(() => {
    let cancelled = false;
    const controller = new AbortController();
    listNodes(controller.signal)
      .then((rows) => {
        if (cancelled) return;
        setNodes(rows);
        if (rows.length > 0) setNodeID(rows[0].id);
      })
      .catch((err) => {
        if (cancelled) return;
        setError(err instanceof ApiError ? err.message : "노드 목록을 불러오지 못했습니다");
      });
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, []);

  function handleGPUChange(count: number) {
    setGPUCount(count);
    const preset = GPU_PRESETS[count];
    if (preset) {
      setMemoryMb(preset.memory_mb);
      setVCPUs(preset.vcpus);
    }
  }

  async function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    setError(null);
    setFieldErrors({});

    const input: CreateInstanceInput = {
      name,
      node_id: nodeID,
      memory_mb: memoryMb,
      vcpus,
      gpu_count: gpuCount,
      ssh_pubkeys: sshPubkey.trim() ? [sshPubkey.trim()] : [],
      image_ref: "",
    };
    const parsed = createInstanceSchema.safeParse(input);
    if (!parsed.success) {
      const errs: Record<string, string> = {};
      for (const issue of parsed.error.issues) {
        const key = issue.path[0];
        if (typeof key === "string") errs[key] = issue.message;
      }
      setFieldErrors(errs);
      return;
    }

    setBusy(true);
    try {
      const inst = await createInstance(parsed.data);
      router.push(`/instances/${inst.id}`);
      router.refresh();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "생성에 실패했습니다");
    } finally {
      setBusy(false);
    }
  }

  return (
    <form
      onSubmit={handleSubmit}
      className="max-w-xl space-y-5 rounded-lg border border-zinc-200 bg-white p-6 dark:border-zinc-800 dark:bg-zinc-900"
      noValidate
    >
      <div className="space-y-1.5">
        <Label htmlFor="name">이름</Label>
        <Input
          id="name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="my-gpu-vm"
          required
        />
        {fieldErrors.name && (
          <p role="alert" className="text-sm text-red-600 dark:text-red-400">
            {fieldErrors.name}
          </p>
        )}
      </div>

      <div className="space-y-1.5">
        <Label htmlFor="node">노드</Label>
        <select
          id="node"
          name="node"
          value={nodeID}
          onChange={(e) => setNodeID(e.target.value)}
          className="block w-full rounded-md border border-zinc-300 bg-white px-3 py-2 text-sm dark:border-zinc-700 dark:bg-zinc-900"
          required
        >
          {nodes == null ? (
            <option value="">불러오는 중…</option>
          ) : nodes.length === 0 ? (
            <option value="">사용 가능한 노드가 없습니다</option>
          ) : (
            nodes.map((n) => (
              <option key={n.id} value={n.id}>
                {n.node_name} (GPU {n.gpu_count})
              </option>
            ))
          )}
        </select>
        {fieldErrors.node_id && (
          <p role="alert" className="text-sm text-red-600 dark:text-red-400">
            {fieldErrors.node_id}
          </p>
        )}
      </div>

      <div className="space-y-1.5">
        <Label>GPU 수</Label>
        <div className="flex gap-2">
          {GPU_CHOICES.map((count) => (
            <button
              key={count}
              type="button"
              onClick={() => handleGPUChange(count)}
              className={`flex-1 rounded-md border px-3 py-2 text-sm transition-colors ${
                gpuCount === count
                  ? "border-zinc-900 bg-zinc-900 text-white dark:border-zinc-100 dark:bg-zinc-100 dark:text-zinc-900"
                  : "border-zinc-300 bg-white text-zinc-700 hover:bg-zinc-50 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-200 dark:hover:bg-zinc-800"
              }`}
            >
              {count}
            </button>
          ))}
        </div>
      </div>

      <div className="grid grid-cols-2 gap-4">
        <div className="space-y-1.5">
          <Label htmlFor="memory">메모리 (MiB)</Label>
          <Input
            id="memory"
            type="number"
            min={512}
            value={memoryMb}
            onChange={(e) => setMemoryMb(Number(e.target.value))}
            required
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="vcpus">vCPU</Label>
          <Input
            id="vcpus"
            type="number"
            min={1}
            value={vcpus}
            onChange={(e) => setVCPUs(Number(e.target.value))}
            required
          />
        </div>
      </div>

      <div className="space-y-1.5">
        <Label htmlFor="ssh">SSH 공개키 (옵션)</Label>
        <textarea
          id="ssh"
          rows={3}
          value={sshPubkey}
          onChange={(e) => setSSHPubkey(e.target.value)}
          placeholder="ssh-ed25519 AAAA..."
          className="block w-full rounded-md border border-zinc-300 bg-white px-3 py-2 text-sm font-mono dark:border-zinc-700 dark:bg-zinc-900"
        />
      </div>

      {error && (
        <p role="alert" className="rounded-md bg-red-50 px-3 py-2 text-sm text-red-700 dark:bg-red-950/30 dark:text-red-400">
          {error}
        </p>
      )}

      <div className="flex gap-2">
        <Button type="submit" disabled={busy || (nodes != null && nodes.length === 0)}>
          {busy ? "생성 중…" : "생성"}
        </Button>
        <Button
          type="button"
          variant="secondary"
          onClick={() => router.back()}
          disabled={busy}
        >
          취소
        </Button>
      </div>
    </form>
  );
}
