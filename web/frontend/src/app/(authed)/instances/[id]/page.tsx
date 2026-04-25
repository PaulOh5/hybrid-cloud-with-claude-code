import { InstanceDetail } from "@/components/instances/instance-detail";

const SSH_HOST = process.env.NEXT_PUBLIC_SSH_HOST ?? "qlaud.net";
const SSH_USERNAME = process.env.NEXT_PUBLIC_SSH_USERNAME ?? "ubuntu";

type Params = Promise<{ id: string }>;

export default async function InstanceDetailPage({ params }: { params: Params }) {
  const { id } = await params;
  return (
    <section className="space-y-6">
      <InstanceDetail instanceID={id} sshHost={SSH_HOST} sshUsername={SSH_USERNAME} />
    </section>
  );
}
