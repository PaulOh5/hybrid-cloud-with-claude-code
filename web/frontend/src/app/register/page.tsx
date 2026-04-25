"use client";

import { useRouter } from "next/navigation";
import Link from "next/link";
import { AuthForm } from "@/components/auth/auth-form";
import { register } from "@/lib/api/auth";

export default function RegisterPage() {
  const router = useRouter();

  return (
    <div className="flex flex-1 items-center justify-center px-4 py-12">
      <AuthForm
        title="회원가입"
        submitLabel="회원가입"
        onSubmit={async (creds) => {
          await register(creds);
          router.push("/instances");
          router.refresh();
        }}
        footer={
          <>
            이미 계정이 있으신가요?{" "}
            <Link href="/login" className="font-medium text-zinc-900 underline dark:text-zinc-100">
              로그인
            </Link>
          </>
        }
      />
    </div>
  );
}
