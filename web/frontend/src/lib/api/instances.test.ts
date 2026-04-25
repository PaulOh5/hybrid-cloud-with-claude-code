import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  createInstance,
  createInstanceSchema,
  deleteInstance,
  getInstance,
  listInstances,
} from "./instances";

const sampleInstance = {
  id: "30c7b44f-d382-4e5e-8fff-b14db3673836",
  node_id: "11111111-2222-4333-8444-555555555555",
  name: "demo",
  state: "running",
  memory_mb: 2048,
  vcpus: 2,
  gpu_count: 1,
  ssh_pubkeys: ["ssh-ed25519 AAAA"],
  vm_internal_ip: "192.168.122.123",
  error_message: "",
  created_at: "2026-04-25T00:00:00Z",
  updated_at: "2026-04-25T00:01:00Z",
};

beforeEach(() => {
  global.fetch = vi.fn() as unknown as typeof fetch;
});

afterEach(() => {
  vi.restoreAllMocks();
});

function mockFetchOnce(status: number, body: unknown) {
  const fetchMock = global.fetch as unknown as ReturnType<typeof vi.fn>;
  fetchMock.mockResolvedValueOnce(
    new Response(body == null ? null : JSON.stringify(body), {
      status,
      headers: body == null ? {} : { "Content-Type": "application/json" },
    }),
  );
}

describe("instances client", () => {
  it("listInstances parses array", async () => {
    mockFetchOnce(200, { instances: [sampleInstance] });
    const got = await listInstances();
    expect(got).toHaveLength(1);
    expect(got[0].name).toBe("demo");
  });

  it("getInstance encodes the id segment", async () => {
    mockFetchOnce(200, { instance: sampleInstance });
    await getInstance(sampleInstance.id);
    const fetchMock = global.fetch as unknown as ReturnType<typeof vi.fn>;
    const [path] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe(`/api/v1/instances/${sampleInstance.id}`);
  });

  it("createInstance posts JSON", async () => {
    mockFetchOnce(201, { instance: sampleInstance });
    await createInstance({
      name: "demo",
      node_id: sampleInstance.node_id,
      memory_mb: 2048,
      vcpus: 2,
      gpu_count: 1,
      ssh_pubkeys: [],
      image_ref: "",
    });
    const fetchMock = global.fetch as unknown as ReturnType<typeof vi.fn>;
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/api/v1/instances");
    expect(init.method).toBe("POST");
  });

  it("deleteInstance resolves on 204", async () => {
    mockFetchOnce(204, null);
    await expect(deleteInstance(sampleInstance.id)).resolves.toBeUndefined();
  });
});

describe("createInstanceSchema", () => {
  it("rejects bad name characters", () => {
    const r = createInstanceSchema.safeParse({
      name: "Bad Name!",
      node_id: "30c7b44f-d382-4e5e-8fff-b14db3673836",
      memory_mb: 1024,
      vcpus: 1,
      gpu_count: 0,
      ssh_pubkeys: [],
      image_ref: "",
    });
    expect(r.success).toBe(false);
  });

  it("rejects non-uuid node_id", () => {
    const r = createInstanceSchema.safeParse({
      name: "ok-name",
      node_id: "not-uuid",
      memory_mb: 1024,
      vcpus: 1,
      gpu_count: 0,
      ssh_pubkeys: [],
      image_ref: "",
    });
    expect(r.success).toBe(false);
  });

  it("accepts a valid input", () => {
    const r = createInstanceSchema.safeParse({
      name: "valid-name-1",
      node_id: "30c7b44f-d382-4e5e-8fff-b14db3673836",
      memory_mb: 2048,
      vcpus: 2,
      gpu_count: 1,
      ssh_pubkeys: ["ssh-ed25519 AAAA"],
      image_ref: "",
    });
    expect(r.success).toBe(true);
  });
});
