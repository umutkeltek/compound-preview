import { SkeletonBlock, SkeletonLine } from "@/components/Skeleton";

export default function CapabilitiesLoading() {
  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div className="flex flex-col gap-2">
          <SkeletonLine className="h-3 w-40" />
          <SkeletonLine className="h-5 w-40" />
        </div>
        <SkeletonBlock className="h-6 w-32 rounded-pill" />
      </div>
      <SkeletonBlock className="h-11 w-full rounded-lg" />
      <div className="flex flex-wrap items-center gap-3">
        <SkeletonBlock className="h-8 w-40 rounded-md" />
        <SkeletonBlock className="ml-auto h-8 w-24 rounded-md" />
      </div>
      <div
        className="flex items-center justify-center rounded-lg border border-hair bg-soft-2 text-sm text-muted"
        style={{ height: 600 }}
      >
        loading capability graph…
      </div>
    </div>
  );
}
