"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { listInstances, type Instance } from "@/lib/api/instances";
import { ApiError } from "@/lib/api/client";
import { StateBadge } from "./state-badge";

const POLL_INTERVAL_MS = 5_000;

export function InstanceList() {
  const [instances, setInstances] = useState<Instance[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    const controller = new AbortController();

    async function refresh() {
      try {
        const rows = await listInstances(controller.signal);
        if (!cancelled) {
          setInstances(rows);
          setError(null);
        }
      } catch (err) {
        if (cancelled || (err instanceof DOMException && err.name === "AbortError")) return;
        setError(err instanceof ApiError ? err.message : "목록을 불러오지 못했습니다");
      }
    }

    void refresh();
    const handle = setInterval(refresh, POLL_INTERVAL_MS);
    return () => {
      cancelled = true;
      controller.abort();
      clearInterval(handle);
    };
  }, []);

  if (error) {
    return (
      <p
        role="alert"
        className="rounded-md bg-red-50 px-3 py-2 text-sm text-red-700 dark:bg-red-950/30 dark:text-red-400"
      >
        {error}
      </p>
    );
  }

  if (instances == null) {
    return <p className="text-sm text-zinc-500 dark:text-zinc-400">불러오는 중…</p>;
  }

  if (instances.length === 0) {
    return (
      <div className="rounded-lg border border-dashed border-zinc-300 px-6 py-12 text-center text-sm text-zinc-500 dark:border-zinc-700 dark:text-zinc-400">
        아직 생성된 인스턴스가 없습니다.{" "}
        <Link href="/instances/new" className="font-medium text-zinc-900 underline dark:text-zinc-100">
          새로 만들기
        </Link>
      </div>
    );
  }

  return (
    <div className="overflow-hidden rounded-lg border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900">
      <table className="w-full text-sm">
        <thead className="bg-zinc-50 dark:bg-zinc-800/50">
          <tr className="text-left text-xs font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400">
            <th className="px-4 py-2">이름</th>
            <th className="px-4 py-2">상태</th>
            <th className="px-4 py-2">GPU</th>
            <th className="px-4 py-2">메모리</th>
            <th className="px-4 py-2">vCPU</th>
            <th className="px-4 py-2 text-right">생성일</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-zinc-100 dark:divide-zinc-800">
          {instances.map((inst) => (
            <tr key={inst.id} className="hover:bg-zinc-50 dark:hover:bg-zinc-800/30">
              <td className="px-4 py-3">
                <Link
                  href={`/instances/${inst.id}`}
                  className="font-medium text-zinc-900 hover:underline dark:text-zinc-100"
                >
                  {inst.name}
                </Link>
              </td>
              <td className="px-4 py-3">
                <StateBadge state={inst.state} />
              </td>
              <td className="px-4 py-3">{inst.gpu_count}</td>
              <td className="px-4 py-3">{inst.memory_mb} MiB</td>
              <td className="px-4 py-3">{inst.vcpus}</td>
              <td className="px-4 py-3 text-right text-zinc-500 dark:text-zinc-400">
                {new Date(inst.created_at).toLocaleString()}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
