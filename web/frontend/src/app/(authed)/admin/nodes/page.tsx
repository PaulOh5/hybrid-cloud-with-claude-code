"use client";

import { useEffect, useState } from "react";
import { DataTable } from "@/components/admin/data-table";
import { adminListNodes } from "@/lib/api/admin";
import type { UserNode } from "@/lib/api/nodes";
import { ApiError } from "@/lib/api/client";

export default function AdminNodesPage() {
  const [rows, setRows] = useState<UserNode[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    const ac = new AbortController();
    adminListNodes(ac.signal)
      .then((r) => {
        if (!cancelled) setRows(r);
      })
      .catch((err) => {
        if (cancelled || (err instanceof DOMException && err.name === "AbortError")) return;
        setError(err instanceof ApiError ? err.message : "노드를 불러오지 못했습니다");
      });
    return () => {
      cancelled = true;
      ac.abort();
    };
  }, []);

  return (
    <section className="space-y-4">
      <h1 className="text-2xl font-semibold tracking-tight">노드</h1>
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
          rowKey={(n) => n.id}
          columns={[
            { header: "이름", cell: (n) => n.node_name },
            { header: "상태", cell: (n) => n.status },
            { header: "GPU", cell: (n) => n.gpu_count, align: "right" },
          ]}
        />
      )}
    </section>
  );
}
