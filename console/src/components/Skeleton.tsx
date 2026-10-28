/**
 * Shared skeleton primitives for route `loading.tsx` files. They reuse the
 * app's existing tokens and never fetch data.
 */
export function SkeletonBlock({ className = "" }: { className?: string }) {
  return <div aria-hidden="true" className={`skeleton ${className}`} />;
}

export function SkeletonLine({ className = "h-3 w-2/3" }: { className?: string }) {
  return <div aria-hidden="true" className={`skeleton rounded-md ${className}`} />;
}

export function SkeletonCard({ className = "" }: { className?: string }) {
  return (
    <div className={`card flex flex-col gap-3 p-5 ${className}`}>
      <SkeletonLine className="h-3 w-1/3" />
      <SkeletonLine className="h-3 w-full" />
      <SkeletonLine className="h-3 w-5/6" />
    </div>
  );
}

export function SkeletonHeader({ withChip = true }: { withChip?: boolean }) {
  return (
    <div className="flex items-end justify-between gap-4">
      <div className="flex flex-col gap-2">
        <SkeletonLine className="h-5 w-40" />
        <SkeletonLine className="h-3 w-64" />
      </div>
      {withChip ? <SkeletonBlock className="h-6 w-28 rounded-full" /> : null}
    </div>
  );
}
