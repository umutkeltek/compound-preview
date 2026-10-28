"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";

import { Button, ConfirmDialog } from "@/components/kit";

/**
 * Fixes /sources copy-only for real source cards (Phase 3b). A governed
 * one-click that runs `source intake-work-plan --card <sicard_id> --json` —
 * the one clean-argv, card-scoped source act — behind a ConfirmDialog. It
 * builds a governed intake work plan (its own PolicyDecision) and is reversible
 * (the planned work can be cancelled). The card's copy command stays as the
 * escape hatch beside it.
 */
export function SourceIntakeButton({
  cardId,
  mutationsEnabled,
}: {
  cardId: string;
  mutationsEnabled: boolean;
}) {
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function run() {
    setPending(true);
    setError(null);
    try {
      const response = await fetch("/api/decision/execute", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ area: "source", itemId: cardId, option: "intake_plan", fields: { actor: "codex" } }),
      });
      const body = (await response.json().catch(() => null)) as { ok?: boolean; stderr?: string } | null;
      if (response.ok && body?.ok) {
        setOpen(false);
        router.refresh();
      } else {
        setError(body?.stderr || `failed (${response.status})`);
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "request failed");
    } finally {
      setPending(false);
    }
  }

  return (
    <div className="flex flex-col items-start gap-1">
      <Button
        variant="primary"
        disabled={!mutationsEnabled}
        onClick={() => setOpen(true)}
        title={mutationsEnabled ? undefined : "Launch via 'compound console --operator' to act from a pinned console."}
      >
        Plan intake work
      </Button>
      <ConfirmDialog
        open={open}
        onOpenChange={setOpen}
        title="Plan intake work"
        body="Builds a governed intake work plan for this source card. Nothing is collected, authed, or scheduled from this card."
        policyNote="Creates a work plan with its own PolicyDecision — reversible (the planned work can be cancelled)."
        argv={["source", "intake-work-plan", "--card", cardId, "--json"]}
        pending={pending}
        confirmLabel="Plan work"
        onConfirm={run}
      />
      {error ? <span className="text-2xs leading-snug text-crit">{error}</span> : null}
    </div>
  );
}
