import { ShieldIcon, StatusChip } from "@/components/kit";
import { arr, num, str, type Json } from "@/lib/compound";

import { HaltDisclosure } from "./HaltDisclosure";
import { Card, CountChip, DoctrineNote, FileOwnedFoot, KV, SettingsSection } from "./primitives";

/**
 * Group 1 — Autonomy & safety (danger-first). Kill-switch status, autonomy
 * ceiling (read-only), standing grants (read-only + honest copy-revoke), and
 * the fail-closed editable-surfaces whitelist. The halt itself is guarded
 * behind type-to-confirm; nothing here fires a mutation.
 */
export function AutonomySafety({
  halt,
  authority,
  grants,
  editable,
  generatedAt,
}: {
  halt: Json | null;
  authority: Json | null;
  grants: Json | null;
  editable: Json | null;
  generatedAt?: string;
}) {
  const haltInstalled = Boolean(halt?.installed);
  const globalHalt = Boolean(halt?.global_halt);
  const disabledClasses = num(halt?.disabled_class_count);
  const armed = globalHalt || disabledClasses > 0;
  const haltPath = str(halt?.authority_path) || "config/autonomy-halt.yaml (under the runtime home)";

  const level = str(authority?.autonomy_level) || "—";
  const mode = str(authority?.mode);
  const authorityInstalled = Boolean(authority?.installed);

  const grantRows = arr(grants?.grants);
  const grantCount = num(grants?.grant_count) || grantRows.length;

  const nothingEditable = Boolean(editable?.nothing_editable);
  const hardDeny = arr(editable?.hard_deny_core).map(String);
  const entryCount = num(editable?.entry_count);
  const editableInstalled = Boolean(editable?.installed);

  return (
    <SettingsSection
      index={1}
      icon={<ShieldIcon />}
      tone="seam"
      title="Autonomy & safety"
      question="Is autonomous action allowed right now, and inside what ceiling?"
      source="compound policy autonomy-halt --json"
      generatedAt={generatedAt}
    >
      {/* Kill-switch — the prominent card. Amber/crit framing only when readable
          state says the halt is armed; honest static explanation otherwise. */}
      <Card
        seamTone={globalHalt ? "crit" : armed ? "seam" : "good"}
        title="Kill switch"
        provenance="governed"
        badge={
          globalHalt ? (
            <StatusChip tone="crit" label="global halt armed" title="compound policy autonomy-halt --json" />
          ) : armed ? (
            <StatusChip tone="seam" label={`${disabledClasses} class halted`} title="compound policy autonomy-halt --json" />
          ) : (
            <StatusChip tone="good" label="clear" title="compound policy autonomy-halt --json" />
          )
        }
      >
        <div className="flex flex-col gap-3">
          <KV k="installed" v={haltInstalled ? "yes" : "no"} />
          <KV k="global_halt" v={globalHalt ? "true" : "false"} />
          <KV k="disabled_class_count" v={disabledClasses} />
          <DoctrineNote>
            {armed
              ? "Autonomy is halted. Executors may not mutate under a halted class until the authority is lifted."
              : "Nothing is halted. The kill switch is a fail-safe, not a toggle — it stays operator-guarded so a stray click can never disable governance."}
          </DoctrineNote>
          <HaltDisclosure inspectCmd="compound policy autonomy-halt --json" authorityPath={haltPath} />
        </div>
      </Card>

      <div className="grid gap-4 md:grid-cols-2">
        {/* Autonomy ceiling — read-only. */}
        <Card title="Autonomy ceiling" badge={<StatusChip tone="muted" label={level} title="autonomy_level (compound policy operator-authority --json)" />}>
          <KV k="autonomy_level" v={level} />
          <KV k="mode" v={mode ? mode.replace(/_/g, " ") : "—"} />
          <KV k="authority installed" v={authorityInstalled ? "yes" : "no"} />
          <DoctrineNote>
            <span className="mt-2 block">
              The ceiling is the highest autonomy compound may reach; mode narrows what it does inside that ceiling.
            </span>
          </DoctrineNote>
          <FileOwnedFoot
            cmd="compound policy import-operator-state <operator-state.json>"
            note="Operator-state is file-owned. The console reads effective authority; change it at the source and re-import."
          />
        </Card>

        {/* Editable-surfaces whitelist — fail-closed. */}
        <Card title="Editable surfaces" badge={<CountChip n={entryCount} label="entries" tone={nothingEditable ? "good" : "muted"} />}>
          <KV k="installed" v={editableInstalled ? "yes" : "no"} />
          <KV k="nothing_editable" v={nothingEditable ? "true (fail-closed)" : "false"} />
          <KV k="hard_deny_core" v={hardDeny.length ? hardDeny.join(", ") : "—"} mono={false} />
          <DoctrineNote>
            <span className="mt-2 block">
              Fail-closed: with no whitelist installed, nothing is editable. The hard-deny core can never be opened,
              whitelist or not.
            </span>
          </DoctrineNote>
          <FileOwnedFoot cmd="compound policy editable-surfaces --json" note="Whitelist is file-owned config at the editable-surfaces authority path." />
        </Card>
      </div>

      {/* Standing grants — read-only table; revoke is an honest copy-command. */}
      <Card title="Standing grants" badge={<CountChip n={grantCount} label="grants" tone={grantCount > 0 ? "seam" : "good"} />}>
        {grantRows.length ? (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-xs">
              <thead>
                <tr className="border-b border-hair text-2xs uppercase tracking-wide text-faint">
                  <th className="py-1.5 pr-3 font-semibold">capability</th>
                  <th className="py-1.5 pr-3 font-semibold">ceiling</th>
                  <th className="py-1.5 pr-3 font-semibold">target</th>
                  <th className="py-1.5 font-semibold">expires</th>
                </tr>
              </thead>
              <tbody className="mono tabular-nums">
                {grantRows.map((grant: Json, index: number) => (
                  <tr key={str(grant.id) || index} className="border-b border-hair last:border-b-0">
                    <td className="py-1.5 pr-3 text-ink">{str(grant.capability) || "—"}</td>
                    <td className="py-1.5 pr-3 text-muted">{str(grant.reversibility_ceiling) || "—"}</td>
                    <td className="max-w-40 truncate py-1.5 pr-3 text-muted" title={str(grant.target_path)}>
                      {str(grant.target_path) || "—"}
                    </td>
                    <td className="py-1.5 text-muted">{str(grant.expires_at) || "never"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <DoctrineNote>No standing grants. Every mutation needs a fresh policy decision.</DoctrineNote>
        )}
        <FileOwnedFoot
          cmd="compound policy import-standing-grants <standing-grants.yaml>"
          note="Grants are file-owned. To revoke, edit the source grants file and re-import — there is no console revoke button on purpose."
        />
      </Card>
    </SettingsSection>
  );
}
