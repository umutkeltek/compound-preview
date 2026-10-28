import { SkeletonBlock, SkeletonLine } from "@/components/Skeleton";

export default function RoutingLoading() {
  return (
    <div className="flex flex-col gap-8">
      <div className="flex items-end justify-between gap-4">
        <div className="flex flex-col gap-2">
          <SkeletonLine className="h-3 w-32" />
          <SkeletonLine className="h-6 w-48" />
        </div>
        <SkeletonBlock className="h-6 w-28 rounded-pill" />
      </div>

      <div className="flex flex-col gap-3">
        <SkeletonLine className="h-4 w-40" />
        {[0, 1].map((i) => (
          <SkeletonBlock key={i} className="h-32 rounded-lg" />
        ))}
      </div>

      <div className="flex flex-col gap-4">
        <SkeletonLine className="h-4 w-56" />
        <SkeletonBlock className="h-64 rounded-lg" />
      </div>
    </div>
  );
}
