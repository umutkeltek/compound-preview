"use client";

import { authorityDisplay } from "@/lib/action-authority";
import type { AuthorityFace } from "@/representation";

export function AuthorityFaceDetails({
  authority,
  embedded = false,
  className = "",
}: {
  authority: AuthorityFace;
  embedded?: boolean;
  className?: string;
}) {
  const display = authorityDisplay(authority);

  return (
    <section
      className={`${embedded ? "" : "mx-4 mb-4"} ${className} rounded-md border border-hair bg-soft px-3 py-3`}
      aria-label={authority.current ? "Current action authority" : "Non-current authority evidence"}
    >
      <div className="text-2xs font-bold uppercase tracking-wide text-faint">
        {authority.current ? "Current action authority" : "Non-current authority evidence"}
      </div>
      <p className="mt-1 text-sm font-semibold leading-relaxed text-ink">{display.wording}</p>
      <dl className="mt-3 grid gap-x-4 gap-y-2 sm:grid-cols-2">
        {display.rows.map(({ label, value }) => (
          <div key={label} className="min-w-0">
            <dt className="text-2xs font-semibold uppercase tracking-wide text-faint">{label}</dt>
            <dd className="mt-0.5 break-words text-xs leading-relaxed text-muted">{value}</dd>
          </div>
        ))}
      </dl>
      <div className="mt-3 border-t border-hair pt-2">
        <div className="text-2xs font-semibold uppercase tracking-wide text-faint">Expected proof</div>
        {display.expectedProof.length ? (
          <ul className="mt-1 flex flex-col gap-1">
            {display.expectedProof.map((proof) => (
              <li key={proof}><code className="mono break-all text-2xs text-muted">{proof}</code></li>
            ))}
          </ul>
        ) : (
          <p className="mt-1 text-xs text-muted">Not specified by owner</p>
        )}
      </div>
    </section>
  );
}
