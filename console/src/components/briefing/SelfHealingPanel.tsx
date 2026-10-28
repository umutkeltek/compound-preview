import { CopyCmd } from "@/components/CopyCmd";
import { EmptyDoctrine, Panel, SectionLabel } from "@/components/briefing/primitives";
import { StatusChip } from "@/components/kit";
import { FeedDegraded } from "@/components/kit/FeedDegraded";
import type { SelfHealing } from "@/lib/briefing";

/**
 * Self-healing panel (redesign plan Part 3 below-the-fold): home-doctor hygiene
 * flags with the exact repair command. The repair verbs are NOT console-
 * certified one-click acts, so this stays honest — it names the drift and hands
 * the operator the command to run, never a fake button. Clean home → doctrine
 * empty state (DESIGN.md §7.9).
 */
export function SelfHealingPanel({ health }: { health: SelfHealing }) {
  const unknown = health.observation === "unreachable" || health.observation === "invalid";
  const stale = health.observation === "stale";
  return (
    <section>
      <SectionLabel
        right={
          unknown ? <StatusChip tone="seam" label="health unknown" /> : stale ? (
            <StatusChip tone="seam" label="last-known result" />
          ) : health.ok ? (
            <StatusChip tone="good" label="healthy" title="compound home doctor --json" />
          ) : (
            <StatusChip tone="seam" label={`${health.flags.length} to heal`} title="compound home doctor --json" />
          )
        }
      >
        Self-healing
      </SectionLabel>

      {unknown ? (
        <Panel className="px-6 py-6">
          <FeedDegraded label="Home health" source="compound home doctor --json" lastGoodAt={health.lastGoodAt} />
          {health.observation === "invalid" ? <p className="mt-3 text-sm text-muted">Doctor report is incomplete. Current health is unknown.</p> : null}
          <CopyCmd cmd="compound home doctor --json" />
        </Panel>
      ) : health.ok || (stale && health.observedOK) ? (
        <Panel className="px-6 py-6">
          <EmptyDoctrine>{stale ? "Last-known home check passed; current health is unconfirmed" : "Clean home. Nothing to heal"}{health.checkedDirs ? ` — ${health.checkedDirs} intact` : ""}.</EmptyDoctrine>
        </Panel>
      ) : (
        <Panel className="overflow-hidden">
          {stale ? <p className="px-5 py-3 text-sm text-seam">Last-known findings. Refresh or run home doctor before acting.</p> : null}
          {health.flags.map((flag, index) => (
            <div
              key={flag.key}
              className={`flex flex-wrap items-center justify-between gap-3 px-5 py-3.5 ${index > 0 ? "border-t border-hair" : ""}`}
              style={{ borderLeftWidth: "3px", borderLeftColor: flag.tone === "crit" ? "var(--crit-fg)" : "var(--seam-fg)" }}
            >
              <div className="flex min-w-0 items-baseline gap-2">
                <span className={`text-md font-bold tabular-nums ${flag.tone === "crit" ? "text-crit" : "text-seam"}`}>
                  {flag.count.toLocaleString("en-US")}
                </span>
                <span className="text-sm text-ink">{flag.label}</span>
              </div>
              <CopyCmd cmd={flag.command} compact />
            </div>
          ))}
        </Panel>
      )}
    </section>
  );
}
