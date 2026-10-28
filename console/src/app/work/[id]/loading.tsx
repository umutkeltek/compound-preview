import { SkeletonBlock, SkeletonCard, SkeletonLine } from "@/components/Skeleton";

export default function WorkDetailLoading() {
  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-col gap-2">
        <SkeletonBlock className="h-5 w-20 rounded-full" />
        <SkeletonLine className="h-6 w-2/3 max-w-xl" />
        <SkeletonLine className="h-3 w-40" />
      </div>

      <div className="work-detail-grid">
        <div className="flex flex-col gap-4">
          <SkeletonCard />
          <SkeletonCard />
          <SkeletonCard />
          <SkeletonCard />
        </div>
        <div className="flex flex-col gap-4">
          <SkeletonCard />
          <SkeletonCard />
          <SkeletonCard />
        </div>
      </div>
    </div>
  );
}
