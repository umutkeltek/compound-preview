import { SkeletonBlock, SkeletonHeader, SkeletonLine } from "@/components/Skeleton";

export default function AgentsLoading() {
  return (
    <div className="flex flex-col gap-6">
      <SkeletonHeader />
      <SkeletonBlock className="h-16 rounded-lg" />
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
        {[0, 1, 2, 3, 4, 5].map((i) => (
          <div key={i} className="rounded-lg border border-hair bg-panel px-3 py-2.5">
            <SkeletonLine className="h-2.5 w-14" />
            <SkeletonBlock className="mt-1 h-5 w-8 rounded-md" />
          </div>
        ))}
      </div>
      <SkeletonBlock className="h-72 rounded-lg" />
      <div className="grid gap-4 lg:grid-cols-2">
        <SkeletonBlock className="h-40 rounded-lg" />
        <SkeletonBlock className="h-40 rounded-lg" />
      </div>
    </div>
  );
}
