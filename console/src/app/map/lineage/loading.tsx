import { SkeletonBlock, SkeletonHeader, SkeletonLine } from "@/components/Skeleton";

export default function LineageIndexLoading() {
  return (
    <div className="flex flex-col gap-6">
      <SkeletonHeader />
      <div className="card p-5">
        <SkeletonLine className="mb-2 h-3 w-full" />
        <SkeletonLine className="mb-4 h-3 w-3/4" />
        <SkeletonBlock className="h-10 w-full rounded-lg" />
      </div>
    </div>
  );
}
