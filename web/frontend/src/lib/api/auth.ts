import { z } from "zod";
import { apiFetch } from "./client";

// User mirrors the JSON shape returned by main-api's authentication
// endpoints. `is_admin` is wire-format snake_case so we keep it raw rather
// than re-mapping in every component.
export const userSchema = z.object({
  id: z.string().uuid(),
  email: z.string().email(),
  is_admin: z.boolean(),
  created_at: z.string(),
});

export type User = z.infer<typeof userSchema>;

const userEnvelope = z.object({ user: userSchema });

export const credentialsSchema = z.object({
  email: z.string().email("올바른 이메일을 입력하세요"),
  password: z
    .string()
    .min(10, "비밀번호는 최소 10자 이상이어야 합니다")
    .max(72, "비밀번호는 72자를 넘을 수 없습니다"),
});

export type Credentials = z.infer<typeof credentialsSchema>;

export async function register(creds: Credentials): Promise<User> {
  const res = await apiFetch<unknown>("/api/v1/auth/register", {
    method: "POST",
    body: creds,
  });
  return userEnvelope.parse(res).user;
}

export async function login(creds: Credentials): Promise<User> {
  const res = await apiFetch<unknown>("/api/v1/auth/login", {
    method: "POST",
    body: creds,
  });
  return userEnvelope.parse(res).user;
}

export async function logout(): Promise<void> {
  await apiFetch<null>("/api/v1/auth/logout", { method: "POST" });
}

export async function me(signal?: AbortSignal): Promise<User> {
  const res = await apiFetch<unknown>("/api/v1/auth/me", { signal });
  return userEnvelope.parse(res).user;
}
