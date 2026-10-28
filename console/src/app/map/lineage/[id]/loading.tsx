import { SkeletonBlock, SkeletonHeader, SkeletonLine } from "@/components/Skeleton";

export default function LineageDetailLoading() {
  return (
    <div className="flex flex-col gap-6">
      <SkeletonHeader />

      <SkeletonBlock className="h-10 w-full rounded-lg" />

      <div className="card p-5">
        <SkeletonLine className="mb-4 h-3 w-16" />
        <div className="flex flex-col gap-4">
          {[0, 1, 2, 3].map((i) => (
            <div key={i} className="flex items-start gap-3">
              <SkeletonBlock className="mt-0.5 h-3 w-3 shrink-0 rounded-full" />
              <div className="flex flex-1 flex-col gap-1.5">
                <SkeletonLine className="h-3 w-1/3" />
                <SkeletonLine className="h-3 w-2/3" />
              </div>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
