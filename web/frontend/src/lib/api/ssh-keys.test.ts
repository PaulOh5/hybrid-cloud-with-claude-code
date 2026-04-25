import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { addSSHKey, addSSHKeySchema, deleteSSHKey, listSSHKeys } from "./ssh-keys";

const sample = {
  id: "30c7b44f-d382-4e5e-8fff-b14db3673836",
  label: "laptop",
  pubkey: "ssh-ed25519 AAAA",
  fingerprint: "abc",
  created_at: "2026-04-25T00:00:00Z",
};

beforeEach(() => {
  global.fetch = vi.fn() as unknown as typeof fetch;
});

afterEach(() => {
  vi.restoreAllMocks();
});

function mockOnce(status: number, body: unknown) {
  const fetchMock = global.fetch as unknown as ReturnType<typeof vi.fn>;
  fetchMock.mockResolvedValueOnce(
    new Response(body == null ? null : JSON.stringify(body), {
      status,
      headers: body == null ? {} : { "Content-Type": "application/json" },
    }),
  );
}

describe("ssh-keys client", () => {
  it("listSSHKeys parses array", async () => {
    mockOnce(200, { ssh_keys: [sample] });
    const got = await listSSHKeys();
    expect(got).toHaveLength(1);
    expect(got[0].label).toBe("laptop");
  });

  it("addSSHKey posts JSON", async () => {
    mockOnce(201, { ssh_key: sample });
    const got = await addSSHKey({ label: "laptop", pubkey: "ssh-ed25519 AAAA" });
    expect(got.id).toBe(sample.id);
  });

  it("deleteSSHKey resolves on 204", async () => {
    mockOnce(204, null);
    await expect(deleteSSHKey(sample.id)).resolves.toBeUndefined();
  });
});

describe("addSSHKeySchema", () => {
  it("rejects empty label", () => {
    expect(addSSHKeySchema.safeParse({ label: "", pubkey: "x" }).success).toBe(false);
  });

  it("rejects empty pubkey", () => {
    expect(addSSHKeySchema.safeParse({ label: "x", pubkey: "" }).success).toBe(false);
  });

  it("accepts good input", () => {
    expect(
      addSSHKeySchema.safeParse({ label: "ok", pubkey: "ssh-ed25519 AAAA" }).success,
    ).toBe(true);
  });
});
