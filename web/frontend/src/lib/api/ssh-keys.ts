import { z } from "zod";
import { apiFetch } from "./client";

export const sshKeySchema = z.object({
  id: z.string().uuid(),
  label: z.string(),
  pubkey: z.string(),
  fingerprint: z.string(),
  created_at: z.string(),
});

export type SSHKey = z.infer<typeof sshKeySchema>;

const listEnvelope = z.object({ ssh_keys: z.array(sshKeySchema) });
const itemEnvelope = z.object({ ssh_key: sshKeySchema });

export const addSSHKeySchema = z.object({
  label: z.string().min(1, "라벨을 입력하세요").max(64, "라벨은 64자 이하여야 합니다"),
  pubkey: z.string().min(1, "공개키를 입력하세요").max(4096, "공개키가 너무 깁니다"),
});

export type AddSSHKeyInput = z.infer<typeof addSSHKeySchema>;

export async function listSSHKeys(signal?: AbortSignal): Promise<SSHKey[]> {
  const data = await apiFetch<unknown>("/api/v1/ssh-keys", { signal });
  return listEnvelope.parse(data).ssh_keys;
}

export async function addSSHKey(input: AddSSHKeyInput): Promise<SSHKey> {
  const data = await apiFetch<unknown>("/api/v1/ssh-keys", {
    method: "POST",
    body: input,
  });
  return itemEnvelope.parse(data).ssh_key;
}

export async function deleteSSHKey(id: string): Promise<void> {
  await apiFetch<null>(`/api/v1/ssh-keys/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}
