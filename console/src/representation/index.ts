/**
 * The representation cluster's frozen public interface (packet
 * 2026-07-11-representation-cluster-v1). EVERYTHING the rest of the console may
 * depend on is re-exported here — importing cluster internals by deep path is a
 * boundary smell. Acts leave only as `ActRequest`; the cluster reaches no act
 * client (enforced by scripts/representation-cluster-guard-smoke.mjs).
 */
export type {
  ActEffect,
  ActRequest,
  ActResult,
  AuthorityFace,
  AuthorityFaceScope,
  AuthorityFaceTarget,
  BoardHeadline,
  Evidence,
  EvidenceAction,
  EvidenceFlow,
  HomeAccount,
  ObjectFace,
  ObjectFaceAction,
  ObjectFaceState,
  RenderResult,
  RepresentationCard,
  ShapeChoice,
  ShapeName,
  ShapeRenderer,
  StateChip,
} from "./types";

export { chooseShape } from "./composer";
export { boardHeadline, homeAccount, ledgerStateFrom, resolveCard, resolveStateChip } from "./laws";
export { actRequestFromAction, listShapes, registerShape, renderRepresentation } from "./registry";
export { Instrument, instrumentActRequest } from "./instruments";
export type { InstrumentKind, InstrumentSetting } from "./instruments";
export { latestProofBackedChange } from "./moments";
export type { ProofBackedChange, ChangeMove } from "./moments";
export { RepresentationRack } from "./RepresentationRack";
