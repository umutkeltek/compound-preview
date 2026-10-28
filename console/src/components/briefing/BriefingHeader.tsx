import { humanAge, isOld } from "@/lib/briefing";
import { str } from "@/lib/compound";

const TZ = "Europe/Istanbul";

function greeting(now: Date): string {
  const hour = Number(new Intl.DateTimeFormat("en-US", { timeZone: TZ, hour: "numeric", hour12: false }).format(now));
  const part = hour < 12 ? "morning" : hour < 18 ? "afternoon" : "evening";
  const name = str(process.env.COMPOUND_OPERATOR_DISPLAY_NAME || process.env.COMPOUND_OPERATOR_NAME);
  return name ? `Good ${part}, ${name}` : `Good ${part}`;
}

function dateLabel(now: Date): string {
  return new Intl.DateTimeFormat("en-US", { timeZone: TZ, weekday: "long", month: "long", day: "numeric" }).format(now);
}

/** N waiting · oldest age. Amber (seam) whenever something waits — this pill is
 * a needs-you signal, so it follows the amber seam grammar (DESIGN.md §0/§2). */
function AttentionPill({ count, oldest }: { count: number; oldest: unknown }) {
  const clear = count <= 0;
  const warn = isOld(oldest);
  const age = humanAge(oldest);
  const tone = clear ? "border-good-line bg-good-soft text-good" : "border-seam-line bg-seam-soft text-seam";
  const dot = clear ? "bg-good" : "bg-seam";
  return (
    <span className={`inline-flex w-fit items-center gap-2 whitespace-nowrap rounded-pill border px-3 py-1 text-xs font-semibold tabular-nums ${tone}`}>
      <span className={`h-1.5 w-1.5 rounded-full ${dot}`} aria-hidden />
      {clear ? (
        "Clear seam"
      ) : (
        <>
          {count.toLocaleString("en-US")} waiting
          {age ? <span className={warn ? "" : "opacity-70"}>· oldest {age}</span> : null}
        </>
      )}
    </span>
  );
}

export function BriefingHeader({ now, needsYouCount, oldest }: { now: Date; needsYouCount: number; oldest: unknown }) {
  return (
    <header className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight text-strong">{greeting(now)}</h1>
        <div className="mt-1 text-sm text-faint">{dateLabel(now)}</div>
      </div>
      <AttentionPill count={needsYouCount} oldest={oldest} />
    </header>
  );
}
