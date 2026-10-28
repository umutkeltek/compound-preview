import { CopyCmd } from "@/components/CopyCmd";

/**
 * Bench — honest doctrine placeholder. `compound bench` is file-based
 * (verified 2026-07-09: `bench --help` → `bench run --suite <file>` /
 * `bench compare --before <report> --after <report>`); it neither lists nor
 * projects "the latest report", and its output carries private ids meant to
 * live under the compound home, never the repo. So the console does NOT fabricate
 * a bench read — it names exactly how a report is produced and hands over the
 * copyable commands. When a cheap `bench report --json` projection lands, this
 * panel becomes a real table; until then it tells the truth.
 */
export function BenchPanel() {
  return (
    <section className="flex flex-col gap-4">
      <div>
        <h2 className="text-md font-semibold text-ink">Bench</h2>
        <p className="mt-0.5 text-2xs leading-relaxed text-muted">
          Does a lane still earn its place? Bench proves it — but the report is file-in / file-out.
        </p>
      </div>

      <div className="rounded-lg border border-dashed border-hair bg-page px-4 py-6">
        <p className="max-w-prose text-sm leading-relaxed text-muted">
          No cheap projection of the latest bench report exists to read. A bench run scores a suite and writes a JSON
          report; a compare diffs two reports. Both take a file and emit a file — nothing the console can safely
          summarize without running a suite. Route a suite, then compare before/after to certify a lane change.
        </p>
        <div className="mt-4 flex flex-col gap-3">
          <div>
            <div className="text-2xs font-bold uppercase tracking-wide text-faint">Run a suite</div>
            <div className="mt-1">
              <CopyCmd cmd="compound bench run --suite <compound-home>/bench-suites/<suite>.json --json" />
            </div>
          </div>
          <div>
            <div className="text-2xs font-bold uppercase tracking-wide text-faint">Compare before / after</div>
            <div className="mt-1">
              <CopyCmd cmd="compound bench compare --before <before>.json --after <after>.json --json" />
            </div>
          </div>
        </div>
        <p className="mt-4 text-2xs leading-relaxed text-faint">
          Reports carry private ids — write them under your compound home, never the repo tree.
        </p>
      </div>
    </section>
  );
}
