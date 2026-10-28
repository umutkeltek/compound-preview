"use client";

import { useState } from "react";

import { CopyCmd } from "@/components/CopyCmd";
import { Button } from "@/components/kit";

/**
 * The guarded kill-switch reveal (redesign plan Part 3, group 1). A global
 * autonomy halt is deliberately NOT a one-click web button — it is armed by
 * writing the halt authority, an operator-executed act. This component enforces
 * that honesty: the operator must type the word "halt" to reveal the exact
 * inspect/engage commands. It NEVER POSTs anything — there is no fabricated act
 * here, only the type-to-confirm disclosure of a copyable command.
 */
export function HaltDisclosure({
  inspectCmd,
  authorityPath,
}: {
  inspectCmd: string;
  authorityPath: string;
}) {
  const [typed, setTyped] = useState("");
  const [open, setOpen] = useState(false);
  const armed = typed.trim().toLowerCase() === "halt";

  if (!open) {
    return (
      <Button variant="danger" onClick={() => setOpen(true)}>
        Reveal halt controls
      </Button>
    );
  }

  return (
    <div className="flex flex-col gap-2.5 rounded-md border border-crit-line bg-crit-soft p-3">
      <p className="text-xs leading-relaxed text-crit">
        A global halt disables autonomous mutation across every class. It is guarded on purpose: the console will
        not fire it. Type <span className="mono font-semibold">halt</span> to reveal the commands.
      </p>
      <input
        type="text"
        value={typed}
        onChange={(event) => setTyped(event.target.value)}
        placeholder="type: halt"
        aria-label="type halt to reveal the kill-switch commands"
        className="mono w-40 rounded-md border border-crit-line bg-panel px-2.5 py-1.5 text-xs text-ink outline-none focus-visible:ring-2 focus-visible:ring-crit-line"
      />
      {armed ? (
        <div className="flex flex-col gap-2">
          <div className="flex flex-col gap-1">
            <span className="text-2xs font-semibold uppercase tracking-wide text-faint">Read current halt state</span>
            <CopyCmd cmd={inspectCmd} />
          </div>
          <div className="flex flex-col gap-1">
            <span className="text-2xs font-semibold uppercase tracking-wide text-faint">Engage (operator-run)</span>
            <p className="text-2xs leading-relaxed text-muted">
              Arming the halt writes the halt authority at{" "}
              <span className="mono break-all text-faint">{authorityPath}</span>. There is no CLI verb and no console
              act for it — that is the fail-safe. Install the authority the same way every other operator authority is
              imported, then re-check state above.
            </p>
          </div>
        </div>
      ) : (
        <span className="text-2xs text-faint">Locked. The commands stay hidden until you type the word.</span>
      )}
    </div>
  );
}
