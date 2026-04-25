import Link from "next/link";
import { Button } from "@/components/ui/button";
import { InstanceList } from "@/components/instances/instance-list";

export default function InstancesPage() {
  return (
    <section className="space-y-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-semibold tracking-tight">인스턴스</h1>
        <Link href="/instances/new">
          <Button>새 인스턴스</Button>
        </Link>
      </div>
      <InstanceList />
    </section>
  );
}
