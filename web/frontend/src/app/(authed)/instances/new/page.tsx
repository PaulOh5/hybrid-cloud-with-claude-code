import { CreateInstanceForm } from "@/components/instances/create-form";

export default function NewInstancePage() {
  return (
    <section className="space-y-6">
      <h1 className="text-2xl font-semibold tracking-tight">새 인스턴스</h1>
      <CreateInstanceForm />
    </section>
  );
}
