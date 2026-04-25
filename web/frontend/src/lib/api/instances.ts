import { z } from "zod";
import { apiFetch } from "./client";

export const instanceSchema = z.object({
  id: z.string().uuid(),
  node_id: z.string().uuid(),
  name: z.string(),
  state: z.enum([
    "pending",
    "provisioning",
    "running",
    "stopping",
    "stopped",
    "failed",
  ]),
  memory_mb: z.number().int(),
  vcpus: z.number().int(),
  gpu_count: z.number().int(),
  ssh_pubkeys: z.array(z.string()),
  vm_internal_ip: z.string().optional().default(""),
  error_message: z.string().optional().default(""),
  created_at: z.string(),
  updated_at: z.string(),
});

export type Instance = z.infer<typeof instanceSchema>;

const instanceListEnvelope = z.object({
  instances: z.array(instanceSchema),
});

const instanceEnvelope = z.object({ instance: instanceSchema });

export async function listInstances(signal?: AbortSignal): Promise<Instance[]> {
  const data = await apiFetch<unknown>("/api/v1/instances", { signal });
  return instanceListEnvelope.parse(data).instances;
}

export async function getInstance(id: string, signal?: AbortSignal): Promise<Instance> {
  const data = await apiFetch<unknown>(`/api/v1/instances/${encodeURIComponent(id)}`, { signal });
  return instanceEnvelope.parse(data).instance;
}

export const createInstanceSchema = z.object({
  name: z
    .string()
    .min(1, "이름을 입력하세요")
    .max(64, "이름은 64자 이하여야 합니다")
    .regex(/^[a-z0-9-]+$/, "영문 소문자, 숫자, 하이픈만 사용 가능합니다"),
  node_id: z.string().uuid("노드를 선택하세요"),
  memory_mb: z.number().int().positive(),
  vcpus: z.number().int().positive(),
  gpu_count: z.number().int().min(0),
  ssh_pubkeys: z.array(z.string()).default([]),
  image_ref: z.string().default(""),
});

export type CreateInstanceInput = z.infer<typeof createInstanceSchema>;

export async function createInstance(input: CreateInstanceInput): Promise<Instance> {
  const data = await apiFetch<unknown>("/api/v1/instances", {
    method: "POST",
    body: input,
  });
  return instanceEnvelope.parse(data).instance;
}

export async function deleteInstance(id: string): Promise<void> {
  await apiFetch<null>(`/api/v1/instances/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}
