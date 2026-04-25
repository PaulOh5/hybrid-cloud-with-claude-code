"use client";

import { useEffect, useState } from "react";
import { DataTable } from "@/components/admin/data-table";
import { adminListSlots, type AdminSlot } from "@/lib/api/admin";
import { ApiError } from "@/lib/api/client";

const statusLabel: Record<AdminSlot["status"], string> = {
  free: "사용 가능",
  reserved: "예약됨",
  in_use: "사용 중",
  draining: "draining",
};

export default function AdminSlotsPage() {
  const [rows, setRows] = useState<AdminSlot[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    const ac = new AbortController();
    adminListSlots(ac.signal)
      .then((r) => {
        if (!cancelled) setRows(r);
      })
      .catch((err) => {
        if (cancelled || (err instanceof DOMException && err.name === "AbortError")) return;
        setError(err instanceof ApiError ? err.message : "슬롯을 불러오지 못했습니다");
      });
    return () => {
      cancelled = true;
      ac.abort();
    };
  }, []);

  return (
    <section className="space-y-4">
      <h1 className="text-2xl font-semibold tracking-tight">GPU 슬롯</h1>
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
          rowKey={(s) => s.id}
          columns={[
            { header: "노드", cell: (s) => s.node_name },
            { header: "Slot #", cell: (s) => s.slot_index, align: "right" },
            { header: "GPU 수", cell: (s) => s.gpu_count, align: "right" },
            { header: "GPU index", cell: (s) => s.gpu_indices.join(", ") },
            { header: "상태", cell: (s) => statusLabel[s.status] },
            {
              header: "사용 인스턴스",
              cell: (s) => (s.current_instance_id ? s.current_instance_id.slice(0, 8) : "—"),
            },
          ]}
        />
      )}
    </section>
  );
}
