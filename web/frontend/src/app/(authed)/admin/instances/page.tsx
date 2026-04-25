"use client";

import { useEffect, useState } from "react";
import { DataTable } from "@/components/admin/data-table";
import { Button } from "@/components/ui/button";
import { adminDeleteInstance, adminListInstances } from "@/lib/api/admin";
import type { Instance } from "@/lib/api/instances";
import { ApiError } from "@/lib/api/client";
import { StateBadge } from "@/components/instances/state-badge";

export default function AdminInstancesPage() {
  const [rows, setRows] = useState<Instance[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [tick, setTick] = useState(0);

  useEffect(() => {
    let cancelled = false;
    const ac = new AbortController();
    adminListInstances(ac.signal)
      .then((r) => {
        if (!cancelled) setRows(r);
      })
      .catch((err) => {
        if (cancelled || (err instanceof DOMException && err.name === "AbortError")) return;
        setError(err instanceof ApiError ? err.message : "인스턴스를 불러오지 못했습니다");
      });
    return () => {
      cancelled = true;
      ac.abort();
    };
  }, [tick]);

  async function handleForceDelete(inst: Instance) {
    if (!confirm(`인스턴스 "${inst.name}"을(를) 강제 종료하시겠습니까?`)) return;
    try {
      await adminDeleteInstance(inst.id);
      setTick((n) => n + 1);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "삭제 실패");
    }
  }

  return (
    <section className="space-y-4">
      <h1 className="text-2xl font-semibold tracking-tight">전체 인스턴스</h1>
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
          rowKey={(i) => i.id}
          columns={[
            { header: "이름", cell: (i) => i.name },
            { header: "ID", cell: (i) => i.id.slice(0, 8) },
            { header: "상태", cell: (i) => <StateBadge state={i.state} /> },
            { header: "GPU", cell: (i) => i.gpu_count, align: "right" },
            { header: "내부 IP", cell: (i) => i.vm_internal_ip || "—" },
            {
              header: "동작",
              cell: (i) => (
                <Button variant="danger" onClick={() => handleForceDelete(i)}>
                  강제 종료
                </Button>
              ),
              align: "right",
            },
          ]}
        />
      )}
    </section>
  );
}
