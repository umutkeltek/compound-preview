import { SkeletonBlock, SkeletonLine } from "@/components/Skeleton";

function Band({ cards, tall = false }: { cards: number; tall?: boolean }) {
  return (
    <section className="flex flex-col gap-3">
      <div className="flex items-baseline justify-between gap-2">
        <SkeletonLine className="h-3 w-32" />
        <SkeletonBlock className="h-5 w-16 rounded-full" />
      </div>
      <SkeletonLine className="h-3 w-72" />
      <div className="grid gap-3 lg:grid-cols-2">
        {Array.from({ length: cards }).map((_, i) => (
          <SkeletonBlock key={i} className={`rounded-xl ${tall ? "h-28" : "h-20"}`} />
        ))}
      </div>
    </section>
  );
}

export default function ReviewsLoading() {
  return (
    <div className="flex flex-col gap-8">
      <div className="flex items-end justify-between gap-4">
        <div className="flex flex-col gap-2">
          <SkeletonLine className="h-5 w-24" />
          <SkeletonLine className="h-3 w-72" />
        </div>
        <SkeletonBlock className="h-6 w-32 rounded-full" />
      </div>
      <Band cards={2} tall />
      <Band cards={4} />
      <Band cards={2} />
    </div>
  );
}
