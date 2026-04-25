import type { ReactNode } from "react";

export type Column<T> = {
  header: string;
  cell: (row: T) => ReactNode;
  align?: "left" | "right";
};

type Props<T> = {
  rows: T[];
  columns: Column<T>[];
  rowKey: (row: T) => string;
  empty?: ReactNode;
};

export function DataTable<T>({ rows, columns, rowKey, empty }: Props<T>) {
  if (rows.length === 0) {
    return (
      <div className="rounded-lg border border-dashed border-zinc-300 px-6 py-12 text-center text-sm text-zinc-500 dark:border-zinc-700 dark:text-zinc-400">
        {empty ?? "데이터가 없습니다."}
      </div>
    );
  }
  return (
    <div className="overflow-x-auto rounded-lg border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900">
      <table className="w-full text-sm">
        <thead className="bg-zinc-50 dark:bg-zinc-800/50">
          <tr className="text-left text-xs font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400">
            {columns.map((c) => (
              <th
                key={c.header}
                className={`px-4 py-2 ${c.align === "right" ? "text-right" : ""}`}
              >
                {c.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="divide-y divide-zinc-100 dark:divide-zinc-800">
          {rows.map((row) => (
            <tr
              key={rowKey(row)}
              className="hover:bg-zinc-50 dark:hover:bg-zinc-800/30"
            >
              {columns.map((c) => (
                <td
                  key={c.header}
                  className={`px-4 py-3 ${c.align === "right" ? "text-right" : ""}`}
                >
                  {c.cell(row)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
