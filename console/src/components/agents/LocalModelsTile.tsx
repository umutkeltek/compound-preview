import { CopyCmd } from "@/components/CopyCmd";
import { FreshnessChip } from "@/components/kit";

/**
 * Local-models lane tile. `compound local-models advise --json` was probed
 * 2026-07-09: with no LM Studio server up it errors fast ("local provider
 * base_url unavailable"), so the console does NOT block a page render on it.
 * This is a static, doctrine tile — the local lane is a batch appliance that
 * wakes on demand (local watts, never an always-on lane), so its readiness is
 * a terminal copy-command, not a live projection. The freshness chip reads
 * "unknown" honestly: there is no fresh projection behind this tile.
 */
export function LocalModelsTile() {
  return (
    <section className="rounded-lg border border-hair bg-panel p-4 shadow-sm">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <h2 className="text-md font-semibold text-ink">Local models</h2>
          <span className="rounded-pill border border-hair bg-transparent px-2 py-0.5 text-xs font-semibold text-muted">
            on demand
          </span>
        </div>
        <FreshnessChip source="compound local-models advise --json" />
      </div>

      <p className="mt-2 text-sm leading-relaxed text-muted">
        The local LM Studio lane is a batch appliance — it wakes on demand for cheap, private extraction on local watts,
        never an always-on lane. Load a model, then ask for advice.
      </p>

      <div className="mt-3">
        <CopyCmd cmd="compound local-models advise --json" />
      </div>
      <p className="mt-2 text-2xs leading-relaxed text-faint">
        Pass <span className="mono text-muted">--lm-studio-url</span> when the local server runs on a non-default
        port.
      </p>
    </section>
  );
}
