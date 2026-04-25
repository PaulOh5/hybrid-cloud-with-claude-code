"use client";

import { useRouter } from "next/navigation";
import Link from "next/link";
import { AuthForm } from "@/components/auth/auth-form";
import { login } from "@/lib/api/auth";

export default function LoginPage() {
  const router = useRouter();

  return (
    <div className="flex flex-1 items-center justify-center px-4 py-12">
      <AuthForm
        title="로그인"
        submitLabel="로그인"
        onSubmit={async (creds) => {
          await login(creds);
          router.push("/instances");
          router.refresh();
        }}
        footer={
          <>
            계정이 없으신가요?{" "}
            <Link href="/register" className="font-medium text-zinc-900 underline dark:text-zinc-100">
              회원가입
            </Link>
          </>
        }
      />
    </div>
  );
}
