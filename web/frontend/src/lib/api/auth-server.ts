import { apiFetchServer } from "./server";
import { userSchema, type User } from "./auth";
import { ApiError } from "./client";
import { z } from "zod";

const userEnvelope = z.object({ user: userSchema });

// requireUser is for Server Components / route handlers — fetches /auth/me
// using the forwarded session cookie. Returns null when unauthenticated; the
// caller decides whether to redirect.
export async function requireUser(): Promise<User | null> {
  try {
    const data = await apiFetchServer<unknown>("/api/v1/auth/me");
    return userEnvelope.parse(data).user;
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) return null;
    throw err;
  }
}
