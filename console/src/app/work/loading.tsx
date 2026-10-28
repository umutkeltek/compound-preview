import { SkeletonBlock, SkeletonHeader, SkeletonLine } from "@/components/Skeleton";

const STAGES = [1, 2, 3, 4, 5, 6, 7];

export default function WorkLoading() {
  return (
    <div className="flex flex-col gap-6">
      <SkeletonHeader />
      <SkeletonBlock className="h-8 w-72 rounded-lg" />
      <div className="overflow-x-auto pb-2">
        <div className="grid gap-3" style={{ gridTemplateColumns: "repeat(7, minmax(178px, 1fr))" }}>
          {STAGES.map((stage) => (
            <div key={stage} className="flex min-w-0 flex-col gap-3">
              <div className="flex items-center gap-2 border-b border-hair pb-2">
                <span className="pipe-node" data-live="empty" aria-hidden />
                <SkeletonLine className="h-3 w-16" />
              </div>
              <SkeletonBlock className="h-20 rounded-lg" />
              <SkeletonBlock className="h-20 rounded-lg" />
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
