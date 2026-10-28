import { arr, num, str, type Json, type ProjectionRead } from "@/lib/compound";

import { FeedDegraded } from "@/components/kit/FeedDegraded";
import { parseHarnessList } from "./data";
import { PlugIcon } from "@/components/kit";

import { Card, CountChip, DoctrineNote, KV, SettingsSection } from "./primitives";

/**
 * Group 5 — Harnesses & hooks. Harness capability facts (parsed from the text
 * `compound harness list`) and the installed-hooks lock summary. Read-only:
 * hooks are clients, not brains — the adapter enforces governance, the harness
 * just carries the fixture.
 */
export function HarnessesHooks({
  harnessText,
  hooks,
  generatedAt,
}: {
  harnessText: string;
  hooks: ProjectionRead;
  generatedAt?: string;
}) {
  const harnesses = parseHarnessList(harnessText);
  const value = hooks.status === "unreachable" ? null : hooks.value;
  const known = typeof value?.installed === "boolean";
  const hookCount = num(value?.hook_count);
  const hookHarnesses = arr(value?.harnesses).map(String);
  const readiness = arr(value?.pre_tool_use_readiness);
  const runtimeReadiness = arr(value?.hook_readiness);

  return (
    <SettingsSection
      index={5}
      icon={<PlugIcon />}
      tone="capability"
      title="Harnesses & hooks"
      question="Which harnesses can compound govern, and where must the adapter enforce the gate?"
      source="compound contract hooks-authority"
      generatedAt={hooks.status === "live" && known ? generatedAt : undefined}
    >
      <Card title="Harness registry" badge={<CountChip n={harnesses.length} label="harnesses" tone="muted" />}>
        {harnesses.length ? (
          <div className="flex flex-col gap-3">
            {harnesses.map((harness) => (
              <div key={harness.name} className="border-b border-hair pb-3 last:border-b-0 last:pb-0">
                <div className="flex flex-wrap items-baseline justify-between gap-2">
                  <span className="mono text-sm font-semibold text-ink">{harness.name}</span>
                  {harness.fallback ? (
                    <span className="mono rounded-pill border border-hair bg-soft px-2 py-0.5 text-2xs text-faint">
                      fallback
                    </span>
                  ) : null}
                </div>
                {harness.capabilities ? (
                  <p className="mt-0.5 text-2xs text-muted">{harness.capabilities}</p>
                ) : null}
                {harness.hooks ? (
                  <p className="mono mt-1 text-2xs leading-relaxed text-faint">{harness.hooks}</p>
                ) : null}
                {harness.limitations.length ? (
                  <ul className="mt-1 flex flex-col gap-0.5">
                    {harness.limitations.map((limitation, index) => (
                      <li key={index} className="text-2xs text-muted">
                        · {limitation}
                      </li>
                    ))}
                  </ul>
                ) : null}
              </div>
            ))}
          </div>
        ) : (
          <pre className="mono overflow-x-auto rounded-md border border-hair bg-soft p-3 text-2xs text-muted">
            {harnessText || "compound harness list produced no output"}
          </pre>
        )}
      </Card>

      <Card title="Installed-hooks lock" badge={known ? <CountChip n={hookCount} label={hooks.status === "stale" ? "last-known hooks" : "hooks"} tone="muted" /> : undefined}>
        {!known ? <FeedDegraded label="Hook authority" source="compound contract hooks-authority" lastGoodAt={hooks.lastGoodAt} /> : null}
        {hooks.status === "stale" ? <p className="mb-3 text-sm text-seam">Last-known hook configuration. Current installation is unconfirmed.</p> : null}
        <KV k="installed harnesses" v={hookHarnesses.length ? hookHarnesses.join(", ") : "—"} mono={false} />
        {readiness.length ? (
          <div className="mt-2 overflow-x-auto">
            <table className="w-full text-left text-xs">
              <thead>
                <tr className="border-b border-hair text-2xs uppercase tracking-wide text-faint">
                  <th className="py-1.5 pr-3 font-semibold">harness</th>
                  <th className="py-1.5 pr-3 font-semibold">pre-tool gate</th>
                  <th className="py-1.5 font-semibold">status</th>
                </tr>
              </thead>
              <tbody className="mono">
                {readiness.map((row: Json, index: number) => {
                  const status = str(row.status);
                  const nativeBlock = status === "native_blocking" && row.native_block === true && row.installed === true;
                  const label = ({
                    native_blocking: "native gate configured",
                    non_blocking: "observation only",
                    not_installed: "not installed",
                    handler_unavailable: "handler unavailable",
                    wrapper_required: "wrapper required",
                  } as Record<string, string>)[status] ?? "unknown";
                  return (
                    <tr key={str(row.harness) || index} className="border-b border-hair last:border-b-0">
                      <td className="py-1.5 pr-3 text-ink">{str(row.harness)}</td>
                      <td className="py-1.5 pr-3">
                        <span className={nativeBlock ? "text-good" : "text-seam"}>
                          {label}
                        </span>
                      </td>
                      <td className="py-1.5 text-muted">{str(row.reason) || status.replace(/_/g, " ")}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        ) : null}
        {runtimeReadiness.length ? (
          <div className="mt-3 border-t border-hair pt-2.5">
            <p className="mb-2 text-xs font-semibold text-ink">Adapter support (not installed enforcement proof)</p>
            {runtimeReadiness.map((row: Json, index) => (
              <p key={index} className="text-xs text-muted">
                {str(row.harness)} · {str(row.hook)}: {str(row.status).replace(/_/g, " ")} · fixture {row.fixture_present === true ? "present" : "absent"} · live adapter {row.live_adapter === true ? "available" : "unavailable"}
              </p>
            ))}
          </div>
        ) : null}
        <div className="mt-3 border-t border-hair pt-2.5">
          <DoctrineNote>
            Interactive hooks observe by default. Registry membership and fixtures do not prove enforcement.
            A guarded profile needs behavioral proof for its exact launch configuration; delegate execution readiness is a separate check.
          </DoctrineNote>
        </div>
      </Card>
    </SettingsSection>
  );
}
