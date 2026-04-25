import { notFound } from "next/navigation";
import Link from "next/link";
import { requireUser } from "@/lib/api/auth-server";

export default async function AdminLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  // Phase 10.1 spec: non-admin access returns 404 (not 403) to avoid
  // exposing the existence of admin routes. requireUser already redirects
  // unauthenticated to /login via (authed)/layout, so by the time we get
  // here we have a user — only need to check is_admin.
  const user = await requireUser();
  if (!user || !user.is_admin) {
    notFound();
  }

  const sections = [
    { href: "/admin/nodes", label: "노드" },
    { href: "/admin/slots", label: "슬롯" },
    { href: "/admin/instances", label: "인스턴스" },
    { href: "/admin/users", label: "사용자" },
  ];

  return (
    <div className="space-y-4">
      <div className="rounded-lg border border-amber-300 bg-amber-50 px-4 py-2 text-sm text-amber-900 dark:border-amber-900 dark:bg-amber-950/30 dark:text-amber-300">
        관리자 모드입니다. 작업이 모든 사용자에게 영향을 줄 수 있습니다.
      </div>
      <nav className="flex flex-wrap gap-2 border-b border-zinc-200 pb-3 text-sm dark:border-zinc-800">
        {sections.map((s) => (
          <Link
            key={s.href}
            href={s.href}
            className="rounded-md px-3 py-1 text-zinc-700 hover:bg-zinc-100 dark:text-zinc-200 dark:hover:bg-zinc-800"
          >
            {s.label}
          </Link>
        ))}
      </nav>
      {children}
    </div>
  );
}
