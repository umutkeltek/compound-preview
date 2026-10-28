import { SkeletonBlock, SkeletonCard, SkeletonHeader, SkeletonLine } from "@/components/Skeleton";

export default function MemoryLoading() {
  return (
    <div className="flex flex-col gap-8">
      <SkeletonHeader />

      <div className="grid gap-4 md:grid-cols-3">
        {[0, 1, 2].map((i) => (
          <div key={i} className="card flex flex-col gap-2 p-5">
            <SkeletonLine className="h-3 w-24" />
            <SkeletonBlock className="h-7 w-12 rounded-md" />
            <SkeletonLine className="h-3 w-28" />
          </div>
        ))}
      </div>

      <SkeletonCard />
      <SkeletonCard />

      <div className="card p-5">
        <SkeletonLine className="mb-4 h-3 w-32" />
        <div className="grid gap-3 lg:grid-cols-2">
          {[0, 1, 2, 3].map((i) => (
            <SkeletonBlock key={i} className="h-24 rounded-xl" />
          ))}
        </div>
      </div>

      <div className="card p-5">
        <SkeletonLine className="mb-4 h-3 w-32" />
        <div className="grid gap-3 lg:grid-cols-2">
          {[0, 1].map((i) => (
            <SkeletonBlock key={i} className="h-24 rounded-xl" />
          ))}
        </div>
      </div>
    </div>
  );
}
