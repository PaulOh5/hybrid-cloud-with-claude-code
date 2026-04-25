"use client";

import { useState, type FormEvent } from "react";
import { credentialsSchema, type Credentials } from "@/lib/api/auth";
import { ApiError } from "@/lib/api/client";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

type Props = {
  // Heading shown above the form ("로그인" or "회원가입").
  title: string;
  submitLabel: string;
  onSubmit: (creds: Credentials) => Promise<void>;
  // Footer is rendered below the submit button — typically a link to the
  // other auth page.
  footer?: React.ReactNode;
};

export function AuthForm({ title, submitLabel, onSubmit, footer }: Props) {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [fieldErrors, setFieldErrors] = useState<{ email?: string; password?: string }>({});
  const [formError, setFormError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    setFieldErrors({});
    setFormError(null);

    const parsed = credentialsSchema.safeParse({ email, password });
    if (!parsed.success) {
      const errs: { email?: string; password?: string } = {};
      for (const issue of parsed.error.issues) {
        const key = issue.path[0];
        if (key === "email") errs.email = issue.message;
        if (key === "password") errs.password = issue.message;
      }
      setFieldErrors(errs);
      return;
    }

    setBusy(true);
    try {
      await onSubmit(parsed.data);
    } catch (err) {
      if (err instanceof ApiError) {
        setFormError(err.message);
      } else {
        setFormError("요청 중 오류가 발생했습니다");
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <form
      onSubmit={handleSubmit}
      className="w-full max-w-sm space-y-5 rounded-lg border border-zinc-200 bg-white p-6 shadow-sm dark:border-zinc-800 dark:bg-zinc-900"
      noValidate
    >
      <h1 className="text-xl font-semibold text-zinc-900 dark:text-zinc-50">{title}</h1>

      <div className="space-y-1.5">
        <Label htmlFor="email">이메일</Label>
        <Input
          id="email"
          name="email"
          type="email"
          autoComplete="email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          required
        />
        {fieldErrors.email && (
          <p role="alert" className="text-sm text-red-600 dark:text-red-400">
            {fieldErrors.email}
          </p>
        )}
      </div>

      <div className="space-y-1.5">
        <Label htmlFor="password">비밀번호</Label>
        <Input
          id="password"
          name="password"
          type="password"
          autoComplete="current-password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          required
        />
        {fieldErrors.password && (
          <p role="alert" className="text-sm text-red-600 dark:text-red-400">
            {fieldErrors.password}
          </p>
        )}
      </div>

      {formError && (
        <p role="alert" className="rounded-md bg-red-50 px-3 py-2 text-sm text-red-700 dark:bg-red-950/30 dark:text-red-400">
          {formError}
        </p>
      )}

      <Button type="submit" disabled={busy} className="w-full">
        {busy ? "처리 중…" : submitLabel}
      </Button>

      {footer && <div className="pt-2 text-center text-sm text-zinc-600 dark:text-zinc-400">{footer}</div>}
    </form>
  );
}
