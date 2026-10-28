import { SkeletonBlock, SkeletonLine } from "@/components/Skeleton";

/** Mirrors the briefing home: greeting + attention chip, insight line, DECIDE rows + rail, KNOWS strip. */
export default function DashboardLoading() {
  return (
    <div className="mx-auto px-7 pt-14 pb-16" style={{ maxWidth: "1080px" }}>
      <header className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
        <div className="flex flex-col gap-2">
          <SkeletonLine className="h-8 w-56" />
          <SkeletonLine className="h-3 w-40" />
        </div>
        <SkeletonBlock className="h-7 w-40 rounded-full" />
      </header>

      <section className="my-12" style={{ maxWidth: "44ch" }}>
        <SkeletonLine className="h-6 w-full" />
        <SkeletonLine className="mt-2 h-6 w-4/5" />
      </section>

      <div
        className="grid gap-7 lg:items-start"
        style={{ gridTemplateColumns: "minmax(0,3fr) minmax(300px,2fr)" }}
      >
        <section>
          <SkeletonLine className="mb-4 h-3 w-20" />
          <div className="overflow-hidden rounded-lg border border-hair bg-panel">
            {[0, 1, 2, 3].map((row) => (
              <div
                key={row}
                className={`flex items-center justify-between gap-4 px-5 py-4 ${row > 0 ? "border-t border-hair" : ""}`}
              >
                <div className="flex min-w-0 flex-1 flex-col gap-2">
                  <SkeletonLine className="h-4 w-3/4" />
                  <SkeletonLine className="h-3 w-1/2" />
                </div>
                <SkeletonBlock className="h-8 w-20 rounded-lg" />
              </div>
            ))}
          </div>
        </section>

        <div className="flex flex-col gap-10">
          {["Agents", "World Delta"].map((label) => (
            <section key={label}>
              <SkeletonLine className="mb-4 h-3 w-16" />
              <div className="flex flex-col gap-2 rounded-lg border border-hair bg-panel p-4">
                <SkeletonLine className="h-3 w-full" />
                <SkeletonLine className="h-3 w-4/5" />
                <SkeletonLine className="h-3 w-3/5" />
              </div>
            </section>
          ))}
        </div>
      </div>

      <section className="mt-14">
        <SkeletonLine className="mb-4 h-3 w-16" />
        <div className="grid gap-6 sm:grid-cols-2 lg:grid-cols-4">
          {[0, 1, 2, 3].map((i) => (
            <div key={i} className="flex flex-col gap-2">
              <SkeletonBlock className="h-7 w-14 rounded-md" />
              <SkeletonLine className="h-3 w-24" />
            </div>
          ))}
        </div>
      </section>
    </div>
  );
}
