// Phase 10.1 admin endpoints. Same shapes as the bearer-token /admin/*
// routes but reachable via the dashboard session cookie. Backend gates with
// is_admin → non-admins get 404 (not 403).

import { z } from "zod";
import { apiFetch } from "./client";
import { instanceSchema, type Instance } from "./instances";
import { userNodeSchema, type UserNode } from "./nodes";

// --- users -----------------------------------------------------------------

export const adminUserSchema = z.object({
  id: z.string().uuid(),
  email: z.string().email(),
  is_admin: z.boolean(),
  balance_milli: z.number().int(),
  active_instance_count: z.number().int(),
  created_at: z.string(),
});

export type AdminUser = z.infer<typeof adminUserSchema>;

const adminUsersEnvelope = z.object({ users: z.array(adminUserSchema) });

export async function adminListUsers(signal?: AbortSignal): Promise<AdminUser[]> {
  const data = await apiFetch<unknown>("/api/v1/admin/users", { signal });
  return adminUsersEnvelope.parse(data).users;
}

// --- slots -----------------------------------------------------------------

export const adminSlotSchema = z.object({
  id: z.string().uuid(),
  node_id: z.string().uuid(),
  node_name: z.string(),
  slot_index: z.number().int(),
  gpu_count: z.number().int(),
  gpu_indices: z.array(z.number().int()),
  nvlink_domain: z.string(),
  status: z.enum(["free", "reserved", "in_use", "draining"]),
  current_instance_id: z.string().uuid().optional(),
});

export type AdminSlot = z.infer<typeof adminSlotSchema>;

const adminSlotsEnvelope = z.object({ slots: z.array(adminSlotSchema) });

export async function adminListSlots(signal?: AbortSignal): Promise<AdminSlot[]> {
  const data = await apiFetch<unknown>("/api/v1/admin/slots", { signal });
  return adminSlotsEnvelope.parse(data).slots;
}

// --- nodes / instances reuse user-side schemas -----------------------------

const adminNodesEnvelope = z.object({ nodes: z.array(userNodeSchema) });

export async function adminListNodes(signal?: AbortSignal): Promise<UserNode[]> {
  const data = await apiFetch<unknown>("/api/v1/admin/nodes", { signal });
  return adminNodesEnvelope.parse(data).nodes;
}

const adminInstancesEnvelope = z.object({ instances: z.array(instanceSchema) });

export async function adminListInstances(signal?: AbortSignal): Promise<Instance[]> {
  const data = await apiFetch<unknown>("/api/v1/admin/instances", { signal });
  return adminInstancesEnvelope.parse(data).instances;
}

export async function adminDeleteInstance(id: string): Promise<void> {
  await apiFetch<null>(`/api/v1/admin/instances/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

// --- credit recharge -------------------------------------------------------

export const adminRechargeSchema = z.object({
  delta_milli: z
    .number()
    .int("정수 milli 값을 입력하세요")
    .refine((v) => v !== 0, "0이 아닌 값을 입력하세요"),
  reason: z.string().min(1, "사유를 입력하세요").max(120),
  idempotency_key: z.string().min(1, "멱등 키를 입력하세요").max(120),
});

export type AdminRechargeInput = z.infer<typeof adminRechargeSchema>;

export async function adminRecharge(userID: string, input: AdminRechargeInput): Promise<void> {
  await apiFetch<unknown>(`/api/v1/admin/users/${encodeURIComponent(userID)}/credits`, {
    method: "POST",
    body: input,
  });
}
