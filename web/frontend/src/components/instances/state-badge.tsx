import type { Instance } from "@/lib/api/instances";

const palette: Record<Instance["state"], string> = {
  pending: "bg-zinc-200 text-zinc-700 dark:bg-zinc-700 dark:text-zinc-200",
  provisioning: "bg-amber-100 text-amber-800 dark:bg-amber-950/40 dark:text-amber-300",
  running: "bg-emerald-100 text-emerald-800 dark:bg-emerald-950/40 dark:text-emerald-300",
  stopping: "bg-amber-100 text-amber-800 dark:bg-amber-950/40 dark:text-amber-300",
  stopped: "bg-zinc-200 text-zinc-700 dark:bg-zinc-700 dark:text-zinc-200",
  failed: "bg-red-100 text-red-800 dark:bg-red-950/40 dark:text-red-300",
};

const labels: Record<Instance["state"], string> = {
  pending: "대기",
  provisioning: "프로비저닝",
  running: "실행 중",
  stopping: "중지 중",
  stopped: "중지됨",
  failed: "실패",
};

export function StateBadge({ state }: { state: Instance["state"] }) {
  return (
    <span
      className={`inline-flex rounded-full px-2 py-0.5 text-xs font-medium ${palette[state]}`}
    >
      {labels[state]}
    </span>
  );
}
