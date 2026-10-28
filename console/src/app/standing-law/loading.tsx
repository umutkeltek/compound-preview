import { SkeletonBlock, SkeletonCard, SkeletonHeader, SkeletonLine } from "@/components/Skeleton";

export default function StandingLawLoading() {
  return (
    <div className="flex flex-col gap-8">
      <SkeletonHeader />

      <div className="flex flex-col gap-3">
        <SkeletonLine className="h-3 w-32" />
        <SkeletonBlock className="h-9 w-80 rounded-lg" />
        <div className="flex flex-col gap-2.5">
          {[0, 1, 2, 3].map((i) => (
            <SkeletonCard key={i} />
          ))}
        </div>
      </div>
    </div>
  );
}
