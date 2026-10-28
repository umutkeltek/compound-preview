import { getInstalledBinaryFreshness } from "@/lib/compound";

export async function SystemBanner({ compact = false }: { compact?: boolean }) {
  const freshness = await getInstalledBinaryFreshness();
  if (freshness.status !== "stale") return null;

  return (
    <div
      title={`${freshness.message} Hooks and live consumers may still run old code.`}
      className={compact
        ? "mx-3 mt-2 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs font-medium text-amber-800 sm:mx-5 md:mx-6"
        : "mr-auto max-w-[620px] truncate rounded-full border border-amber-200 bg-amber-50 px-3 py-1 text-[11.5px] font-medium text-amber-800"}
    >
      {compact ? "Installed compound is stale. Run " : "Installed compound is stale; hooks may not see latest code. Run "}
      <code className="mono">make install</code>.
    </div>
  );
}
