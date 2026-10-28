/**
 * The representation registry — the frozen interface of the volatility cluster.
 * Mirrors `lib/view-registry.ts`: a compound-owned map that turns an untrusted
 * reference into a real element or a plain error, never a throw. The difference
 * from view-registry is deliberate: this registry owns the SHAPE vocabulary and
 * the ACT SEAM, so acts leave the cluster only as an `ActRequest`. It imports no
 * act client — that is what the FR5 guard enforces
 * (scripts/representation-cluster-guard-smoke.mjs).
 */
import { createElement } from "react";

import { chooseShape } from "./composer";
import { SHAPES } from "./shapes";
import type { ActRequest, Evidence, EvidenceAction, RenderResult, ShapeName, ShapeRenderer } from "./types";

const registry = new Map<ShapeName, ShapeRenderer>();

export function registerShape(name: ShapeName, renderer: ShapeRenderer): void {
  if (registry.has(name)) {
    throw new Error(`representation: shape "${name}" is already registered`);
  }
  registry.set(name, renderer);
}

export function listShapes(): ShapeName[] {
  return [...registry.keys()];
}

function registerBuiltinShapes(): void {
  for (const name of Object.keys(SHAPES) as ShapeName[]) {
    if (!registry.has(name)) registry.set(name, SHAPES[name]);
  }
}

registerBuiltinShapes();

/**
 * THE ACT SEAM. The one function that turns an evidence action into an
 * `ActRequest`. The cluster emits ActRequests; a consumer outside the cluster
 * dispatches them onto the governed act path. This indirection is the whole
 * point of the cluster boundary — the representation layer can never mutate on
 * its own.
 */
export function actRequestFromAction(action: EvidenceAction): ActRequest {
  return {
    area: action.area,
    itemId: action.itemId,
    actionId: action.actionId,
    effect: action.effect,
    requiresConfirmation: action.requiresConfirmation,
    expectedRevision: action.expectedRevision,
  };
}

/**
 * Compose one evidence into a representation: pick the shape (composer),
 * render it, and surface any act it carries as ActRequests. Never throws — a
 * missing renderer or a bad evidence is an `{ok:false}` error string.
 */
export function renderRepresentation(evidence: Evidence): RenderResult {
  try {
    const { shape, reason } = chooseShape(evidence);
    const renderer = registry.get(shape) ?? registry.get("story");
    if (!renderer) {
      return { ok: false, error: "representation: no story fallback registered" };
    }
    const element = createElement(renderer, { evidence, reason });
    const actRequests = evidence.action ? [actRequestFromAction(evidence.action)] : [];
    return { ok: true, element, shape, reason, actRequests };
  } catch (err) {
    return {
      ok: false,
      error: `representation: render failed for "${evidence.id}": ${err instanceof Error ? err.message : String(err)}`,
    };
  }
}
