import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { credentialsSchema, login, logout, me, register } from "./auth";
import { ApiError } from "./client";

type FetchMock = ReturnType<typeof vi.fn> & { mockClear: () => void };

const userBody = {
  user: {
    id: "30c7b44f-d382-4e5e-8fff-b14db3673836",
    email: "alice@example.com",
    is_admin: false,
    created_at: "2026-04-25T00:00:00Z",
  },
};

beforeEach(() => {
  global.fetch = vi.fn() as unknown as typeof fetch;
});

afterEach(() => {
  vi.restoreAllMocks();
});

function mockFetchOnce(status: number, body: unknown) {
  const fetchMock = global.fetch as unknown as FetchMock;
  fetchMock.mockResolvedValueOnce(
    new Response(body == null ? null : JSON.stringify(body), {
      status,
      headers: body == null ? {} : { "Content-Type": "application/json" },
    }),
  );
}

describe("credentialsSchema", () => {
  it("rejects short passwords", () => {
    const r = credentialsSchema.safeParse({ email: "a@b.com", password: "short" });
    expect(r.success).toBe(false);
  });

  it("rejects bad emails", () => {
    const r = credentialsSchema.safeParse({ email: "no-at-sign", password: "longenough01" });
    expect(r.success).toBe(false);
  });

  it("accepts good input", () => {
    const r = credentialsSchema.safeParse({ email: "a@b.com", password: "longenough01" });
    expect(r.success).toBe(true);
  });
});

describe("auth client", () => {
  it("register sends POST and parses response", async () => {
    mockFetchOnce(201, userBody);
    const u = await register({ email: "alice@example.com", password: "longenough01" });
    expect(u.email).toBe("alice@example.com");

    const fetchMock = global.fetch as unknown as ReturnType<typeof vi.fn>;
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/api/v1/auth/register");
    expect(init.method).toBe("POST");
    expect(init.credentials).toBe("include");
  });

  it("login parses user", async () => {
    mockFetchOnce(200, userBody);
    const u = await login({ email: "alice@example.com", password: "longenough01" });
    expect(u.id).toBe(userBody.user.id);
  });

  it("login throws ApiError on 401", async () => {
    mockFetchOnce(401, {
      error: { code: "invalid_credentials", message: "invalid email or password" },
    });
    await expect(
      login({ email: "alice@example.com", password: "longenough01" }),
    ).rejects.toMatchObject({
      status: 401,
      code: "invalid_credentials",
    });
  });

  it("logout sends POST and resolves on 204", async () => {
    mockFetchOnce(204, null);
    await expect(logout()).resolves.toBeUndefined();
  });

  it("me returns user", async () => {
    mockFetchOnce(200, userBody);
    const u = await me();
    expect(u.email).toBe("alice@example.com");
  });

  it("ApiError exposes status + code", () => {
    const err = new ApiError(409, "email_taken", "email already registered");
    expect(err.status).toBe(409);
    expect(err.code).toBe("email_taken");
  });
});
