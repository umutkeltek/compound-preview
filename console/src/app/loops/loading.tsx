import { SkeletonBlock, SkeletonLine } from "@/components/Skeleton";

export default function LoopsLoading() {
  return (
    <div className="flex flex-col gap-6">
      <div className="flex items-end justify-between gap-4">
        <div className="flex flex-col gap-2">
          <SkeletonLine className="h-3 w-28" />
          <SkeletonLine className="h-6 w-48" />
        </div>
        <SkeletonBlock className="h-6 w-32 rounded-pill" />
      </div>

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {[0, 1, 2, 3].map((i) => (
          <div key={i} className="flex flex-col gap-2 rounded-lg border border-hair bg-panel p-3 shadow-sm">
            <SkeletonLine className="h-3 w-16" />
            <SkeletonBlock className="h-6 w-10 rounded-md" />
          </div>
        ))}
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        {[0, 1].map((i) => (
          <SkeletonBlock key={i} className="h-64 rounded-lg" />
        ))}
      </div>
    </div>
  );
}
