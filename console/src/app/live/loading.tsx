import { SkeletonBlock, SkeletonHeader, SkeletonLine } from "@/components/Skeleton";

export default function LiveLoading() {
  return (
    <div className="flex flex-col gap-6" aria-label="Loading live activity">
      <SkeletonHeader />
      <SkeletonLine className="h-4 max-w-2xl" />
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        {Array.from({ length: 4 }, (_, index) => <SkeletonBlock key={index} className="h-20 rounded-lg" />)}
      </div>
      <SkeletonBlock className="h-[560px] w-full rounded-xl" />
    </div>
  );
}
