import { redirect } from "next/navigation";
import Link from "next/link";
import { requireUser } from "@/lib/api/auth-server";
import { LogoutButton } from "@/components/auth/logout-button";

export default async function AuthedLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  const user = await requireUser();
  if (!user) {
    redirect("/login");
  }

  return (
    <>
      <header className="border-b border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900">
        <div className="mx-auto flex max-w-5xl items-center justify-between px-4 py-3">
          <Link href="/instances" className="font-semibold text-zinc-900 dark:text-zinc-100">
            Hybrid Cloud
          </Link>
          <nav className="flex items-center gap-4 text-sm">
            <Link
              href="/instances"
              className="text-zinc-600 hover:text-zinc-900 dark:text-zinc-400 dark:hover:text-zinc-100"
            >
              인스턴스
            </Link>
            <Link
              href="/settings/ssh-keys"
              className="text-zinc-600 hover:text-zinc-900 dark:text-zinc-400 dark:hover:text-zinc-100"
            >
              SSH 키
            </Link>
            <span className="text-zinc-500 dark:text-zinc-500">{user.email}</span>
            <LogoutButton />
          </nav>
        </div>
      </header>
      <main className="mx-auto w-full max-w-5xl flex-1 px-4 py-6">{children}</main>
    </>
  );
}
