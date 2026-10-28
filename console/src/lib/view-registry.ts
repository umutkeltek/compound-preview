import { createElement } from "react";
import type { ComponentType, ReactElement } from "react";
import { z } from "zod";

import { FreshnessChip, StateBadge } from "@/components/kit";
import { Stat } from "@/components/ui";
import {
  AskCausalBrief,
  AskComparisonBrief,
  AskWorkList,
} from "@/components/ask/AskViews";

/**
 * The view registry (Plans/i-am-not-happy-rosy-floyd.md Part 6 / Phase 5):
 * compound-owned, lives OUTSIDE `src/ai-console`. Doctrine: the LLM composes
 * the view; the registry owns the act. A `ViewProposal.components` entry is
 * just `{ view: string; props: unknown }` — this module is what turns that
 * untrusted reference into a real, type-checked React element (or a plain
 * error string, never a throw).
 *
 * Generalizes the decision-registry pattern (action-composition) to
 * view-composition, and doubles as the component-kit's type layer.
 */

export interface RegisteredView<T> {
  name: string;
  description: string;
  propsSchema: z.ZodType<T>;
  component: ComponentType<T>;
}

/**
 * Heterogeneous by design: entries close over different `T`s, so the map can
 * only be stored as `any` internally (each entry's own `propsSchema`/
 * `component` pair stays sound — `registerView<T>` is what keeps them
 * matched). Callers only ever see `getView`/`renderViewInvocation`, which
 * type as `unknown`, never `any`.
 */
const registry = new Map<string, RegisteredView<any>>(); // eslint: any is intentional here, see comment above

export function registerView<T>(view: RegisteredView<T>): void {
  if (registry.has(view.name)) {
    throw new Error(`view-registry: "${view.name}" is already registered`);
  }
  registry.set(view.name, view);
}

export function getView(name: string): RegisteredView<unknown> | undefined {
  return registry.get(name);
}

export function listViews(): RegisteredView<unknown>[] {
  return [...registry.values()];
}

/* ---------- built-in views ---------- */

/** Wraps `ui.tsx`'s `Stat` down to the JSON-serializable subset a proposal can carry. */
type StatTileProps = {
  label: string;
  value: string | number;
  sub?: string;
};

function StatTileView({ label, value, sub }: StatTileProps): ReactElement {
  return createElement(Stat, { label, value, sub });
}

function registerBuiltinViews(): void {
  registerView({
    name: "state-badge",
    description: "The one status-pill grammar — renders any lifecycle state string as its tone.",
    propsSchema: z.object({ state: z.string() }),
    component: StateBadge,
  });

  registerView({
    name: "freshness-chip",
    description: "Provenance dot + mono freshness label (every card declares provenance).",
    propsSchema: z.object({
      generatedAt: z.string().optional(),
      source: z.string().optional(),
      staleAfterMs: z.number().optional(),
    }),
    component: FreshnessChip,
  });

  registerView({
    name: "stat-tile",
    description: "Labeled stat card — wraps ui.tsx's Stat down to label/value/sub.",
    propsSchema: z.object({
      label: z.string(),
      value: z.union([z.string(), z.number()]),
      sub: z.string().optional(),
    }),
    component: StatTileView,
  });

  registerView({
    name: "causal-brief",
    description: "Attention-ranked seam with its owner-projected unlock and risk shape.",
    propsSchema: z.object({
      title: z.string(),
      summary: z.string(),
      unlock: z.string().optional(),
      count: z.number(),
      blastRadius: z.string().optional(),
      reversibility: z.string().optional(),
      generatedAt: z.string().optional(),
      source: z.string().optional(),
    }),
    component: AskCausalBrief,
  });

  registerView({
    name: "comparison-brief",
    description: "Two-path comparison that leaves missing owner consequences explicitly unknown.",
    propsSchema: z.object({
      title: z.string(),
      actLabel: z.string(),
      actConsequence: z.string().optional(),
      waitConsequence: z.string().optional(),
      evidenceCount: z.number().optional(),
      generatedAt: z.string().optional(),
      source: z.string().optional(),
    }),
    component: AskComparisonBrief,
  });

  registerView({
    name: "work-list",
    description: "Compact trace-linked list of real WorkEnvelopes.",
    propsSchema: z.object({
      label: z.string(),
      items: z.array(z.object({
        id: z.string(),
        title: z.string(),
        state: z.string(),
        owner: z.string().optional(),
      })),
    }),
    component: AskWorkList,
  });
}

registerBuiltinViews();

/* ---------- invocation ---------- */

export type ViewInvocationResult = { ok: true; element: ReactElement } | { ok: false; error: string };

/**
 * Validate an untrusted `{view, props}` reference against the registry and
 * return either a real element or an error string. Never throws — a bad
 * `view` name, a props mismatch, or a render-time error are all just an
 * `{ok: false}`, because this is the seam an LLM-composed proposal crosses.
 */
export function renderViewInvocation(inv: { view: string; props: unknown }): ViewInvocationResult {
  // Deliberately reads the raw (still-`any`) map entry rather than the public
  // `getView` (typed `unknown`): `parsed.data` below must line up with
  // `view.component`'s own prop type, which `unknown` can't express.
  const view = registry.get(inv.view);
  if (!view) {
    return { ok: false, error: `view-registry: unknown view "${inv.view}"` };
  }

  const parsed = view.propsSchema.safeParse(inv.props);
  if (!parsed.success) {
    return {
      ok: false,
      error: `view-registry: invalid props for "${inv.view}": ${parsed.error.message}`,
    };
  }

  try {
    return { ok: true, element: createElement(view.component, parsed.data) };
  } catch (err) {
    return {
      ok: false,
      error: `view-registry: render failed for "${inv.view}": ${err instanceof Error ? err.message : String(err)}`,
    };
  }
}
