import { z } from "zod";
import { apiFetch } from "./client";

export const userNodeSchema = z.object({
  id: z.string().uuid(),
  node_name: z.string(),
  status: z.string(),
  gpu_count: z.number().int(),
});

export type UserNode = z.infer<typeof userNodeSchema>;

const envelope = z.object({ nodes: z.array(userNodeSchema) });

export async function listNodes(signal?: AbortSignal): Promise<UserNode[]> {
  const data = await apiFetch<unknown>("/api/v1/nodes", { signal });
  return envelope.parse(data).nodes;
}
