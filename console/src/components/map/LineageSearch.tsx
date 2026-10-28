"use client";

import { useRouter } from "next/navigation";
import { FormEvent, useState } from "react";

export function LineageSearch({ initial = "" }: { initial?: string }) {
  const router = useRouter();
  const [q, setQ] = useState(initial);

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const id = q.trim();
    if (id) router.push(`/map/lineage/${encodeURIComponent(id)}`);
  }

  return (
    <form onSubmit={submit} className="flex gap-2">
      <input
        value={q}
        onChange={(e) => setQ(e.target.value)}
        placeholder="trace: cand_… · cl_… · ev_… · work_… · or an entity token"
        className="min-w-0 flex-1 rounded-lg border border-line bg-white px-3 py-2 text-[12.5px] text-ink outline-none focus:border-slate-300"
      />
      <button
        type="submit"
        className="rounded-lg bg-ink px-3 py-2 text-[12px] font-semibold text-white"
      >
        Trace
      </button>
    </form>
  );
}
