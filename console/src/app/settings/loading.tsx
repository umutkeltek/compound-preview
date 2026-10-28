import { SkeletonBlock, SkeletonCard, SkeletonLine } from "@/components/Skeleton";

/** Mirrors the Settings page: breadcrumb + title, then seven grouped sections
 * each a header row + a two-card grid. Token-only, no data fetch. */
export default function SettingsLoading() {
  return (
    <div className="flex flex-col gap-12">
      <div className="flex flex-col gap-2">
        <SkeletonLine className="h-3 w-32" />
        <SkeletonLine className="h-6 w-40" />
      </div>

      {[0, 1, 2, 3, 4, 5, 6].map((group) => (
        <section key={group} className="flex flex-col gap-4">
          <div className="flex items-end justify-between gap-3 border-b border-hair pb-2.5">
            <div className="flex flex-col gap-2">
              <SkeletonLine className="h-4 w-48" />
              <SkeletonLine className="h-3 w-64" />
            </div>
            <SkeletonBlock className="h-6 w-28 rounded-pill" />
          </div>
          <div className="grid gap-4 md:grid-cols-2">
            <SkeletonCard />
            <SkeletonCard />
          </div>
        </section>
      ))}
    </div>
  );
}
