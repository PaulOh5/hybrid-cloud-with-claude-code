// Server-side helpers: read the session cookie from incoming Next.js request
// headers and forward it to main-api so route group layouts can perform auth
// checks at render time. This is only used in Server Components — the client
// path uses lib/api/auth via fetch + credentials:include.

import { cookies, headers } from "next/headers";
import { ApiError } from "./client";

const SESSION_COOKIE = "hc_session";

export type ServerFetchOpts = {
  method?: string;
  body?: unknown;
};

// apiFetchServer mirrors lib/api/client#apiFetch but resolves the absolute
// URL of main-api through MAIN_API_URL (server-only env). The browser path
// uses relative /api/v1/* + same-origin cookies; on the server we need the
// absolute URL to reach the API container.
export async function apiFetchServer<T>(path: string, opts: ServerFetchOpts = {}): Promise<T> {
  const base = process.env.MAIN_API_URL ?? "http://localhost:8080";
  const sessionCookie = (await cookies()).get(SESSION_COOKIE);
  // Forward the X-Forwarded-For header so rate limiting at main-api sees the
  // client IP rather than this server's loopback address.
  const incoming = await headers();
  const forwardedFor =
    incoming.get("x-forwarded-for") ?? incoming.get("x-real-ip") ?? undefined;

  const reqHeaders: Record<string, string> = { Accept: "application/json" };
  if (sessionCookie) {
    reqHeaders["Cookie"] = `${sessionCookie.name}=${sessionCookie.value}`;
  }
  if (forwardedFor) reqHeaders["X-Forwarded-For"] = forwardedFor;
  let body: BodyInit | undefined;
  if (opts.body !== undefined) {
    reqHeaders["Content-Type"] = "application/json";
    body = JSON.stringify(opts.body);
  }

  const res = await fetch(base + path, {
    method: opts.method ?? "GET",
    headers: reqHeaders,
    body,
    cache: "no-store",
  });
  if (!res.ok) {
    let code = "request_failed";
    let message = res.statusText;
    try {
      const data = (await res.json()) as { error?: { code?: string; message?: string } };
      if (data?.error?.code) code = data.error.code;
      if (data?.error?.message) message = data.error.message;
    } catch {
      // ignore non-JSON bodies
    }
    throw new ApiError(res.status, code, message);
  }
  if (res.status === 204) return null as T;
  return (await res.json()) as T;
}
