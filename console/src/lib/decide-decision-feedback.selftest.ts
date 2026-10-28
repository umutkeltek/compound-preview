import fs from "node:fs";
import path from "node:path";

import { fieldValidationMessage, RATIONALE_REQUIRED_MESSAGE } from "./governed-action-client";
import { blockedReasonPlain, ownerUnavailableMessage } from "./glossary";
import { queueSystemicBlocker } from "./decide-members";

/**
 * Falsifiers for the Decide-page presentation bugs the operator
 * browser-reproduced: (1) clicking a primary act with an empty required
 * field silently did nothing — no visible feedback; (2) 11 "Owner action
 * unavailable" cards named no owner and no operator escape hatch; (3) the
 * final confirmation omitted the rationale and other field values it would
 * send.
 *
 * This repo's console tests are pure-function `.selftest.ts` files (no DOM
 * renderer is wired in — see decide-model-brief.selftest.ts, ask-triage.
 * selftest.ts). So this file proves two things for each bug: (a) the pure
 * copy function computes the right plain-language text, and (b) the
 * component source files actually CALL that function at the exact site that
 * used to silently no-op / render the bare label — a grep-on-source wiring
 * proof (same pattern as ask-triage.selftest.ts), so a correct-but-unused
 * helper cannot pass this test the way it could pass a function-only test.
 */

const SRC_ROOT = path.resolve(process.cwd(), "src");

function read(relPath: string): string {
  return fs.readFileSync(path.join(SRC_ROOT, relPath), "utf8");
}

export type SelfTestResult = { ok: boolean; details: string[] };

export function runDecideDecisionFeedbackSelfTest(): SelfTestResult {
  const details: string[] = [];
  let ok = true;
  const fail = (msg: string) => { ok = false; details.push(`FAIL: ${msg}`); };
  const pass = (msg: string) => details.push(`ok: ${msg}`);

  /* ---------- f1: empty-required-field validation feedback ---------- */

  // (a) The rationale/reason textbox gets the plain-language line the
  // operator asked for, not a terse "<Label> is required." fragment.
  {
    const msg = fieldValidationMessage({ name: "rationale", label: "Rationale" });
    if (msg !== RATIONALE_REQUIRED_MESSAGE || !/reasoning/i.test(msg) || !/why/i.test(msg)) {
      fail(`rationale field validation message not plain-language: ${JSON.stringify(msg)}`);
    } else {
      pass("empty rationale field gets the plain-language 'add one line of reasoning' message");
    }
    const reasonMsg = fieldValidationMessage({ name: "reason", label: "Reason" });
    if (reasonMsg !== RATIONALE_REQUIRED_MESSAGE) {
      fail(`reason field should share the rationale copy, got ${JSON.stringify(reasonMsg)}`);
    } else {
      pass("empty reason field shares the same plain-language copy as rationale");
    }
  }

  // (b) Other required fields (e.g. numeric confidence) still get a message
  // — never silently blocked — but distinct generic copy.
  {
    const msg = fieldValidationMessage({ name: "confidence", label: "Confidence" });
    if (!msg || msg === RATIONALE_REQUIRED_MESSAGE || !/Confidence/.test(msg)) {
      fail(`non-rationale required field must still get a labeled message, got ${JSON.stringify(msg)}`);
    } else {
      pass("non-rationale required fields still surface a labeled plain-language message");
    }
  }

  // (c) GovernedAct.tsx (the single-item "Review decision" primary act) must
  // actually WIRE fieldValidationMessage into its validation path and render
  // it as an inline errorMessage on the Field itself — not just import it
  // unused. On base this call site set `message` to the bare
  // `${field.label} is required.` template literal directly; that inline
  // silent-feedback pattern must be gone.
  {
    const src = read("components/decide/GovernedAct.tsx");
    if (!/fieldValidationMessage\(field\)/.test(src)) {
      fail("GovernedAct.tsx validateDraft() no longer calls fieldValidationMessage(field) — the plain-language copy is unwired");
    } else if (/setMessage\(`\$\{field\.label\} is required\.`\)/.test(src)) {
      fail("GovernedAct.tsx still sets the old terse '${field.label} is required.' message — not replaced");
    } else {
      pass("GovernedAct.tsx's validateDraft() computes the message via fieldValidationMessage(field)");
    }
    if (!/errorMessage=\{invalidField === field\.name \? message/.test(src)) {
      fail("GovernedAct.tsx does not pass the validation message to <Field> as errorMessage — feedback would not render next to the field");
    } else {
      pass("GovernedAct.tsx passes the invalid field's message to <Field errorMessage=…> so it renders next to the field, not just far below the button");
    }
  }

  // (d) Field.tsx (the shared form-control kit component) must render that
  // errorMessage directly inside the same <label> as the control — i.e.
  // "next to the field", not merely somewhere on the page.
  {
    const src = read("components/kit/Field.tsx");
    if (!/errorMessage\?:\s*string/.test(src)) {
      fail("Field.tsx has no errorMessage prop — no per-field inline message contract exists");
    } else if (!/invalid && errorMessage/.test(src)) {
      fail("Field.tsx does not conditionally render the errorMessage when invalid");
    } else {
      pass("Field.tsx renders the inline errorMessage directly under its own control when invalid");
    }
  }

  // (e) The batch-enumerable path (EnumerableApply's Rationale textarea +
  // Confirm button) had a DEAD validation check: `apply()` already tested
  // `requiresRationale && !rationale.trim()`, but the Confirm button was
  // disabled by that exact same condition — so the check could never run
  // and the operator saw a button that just didn't respond. Prove the
  // disablement no longer includes the rationale condition (so the click
  // reaches apply() and its existing check can actually fire), and that the
  // message it sets matches the plain-language copy.
  {
    const src = read("components/decide/DecideQueue.tsx");
    if (!/setError\(RATIONALE_REQUIRED_MESSAGE\)/.test(src)) {
      fail("DecideQueue.tsx EnumerableApply no longer sets the plain-language RATIONALE_REQUIRED_MESSAGE on empty rationale");
    } else {
      pass("DecideQueue.tsx EnumerableApply surfaces RATIONALE_REQUIRED_MESSAGE when rationale is empty");
    }
    if (/disabled=\{running \|\| \(requiresRationale && !rationale\.trim\(\)\)\}/.test(src)) {
      fail("DecideQueue.tsx Confirm button is still disabled purely by empty rationale — apply()'s validation check stays dead code and the click silently no-ops");
    } else {
      pass("DecideQueue.tsx Confirm button no longer silently blocks on empty rationale — clicking it reaches the validation check that shows the message");
    }
  }

  // (e2) Enumerable owner actions must name singleton work directly and
  // describe tracked actions as planning, not authority application. The
  // owner-projected policy note stays visible before confirmation and is
  // included in the dialog description announced by Radix.
  {
    const src = read("components/decide/DecideQueue.tsx");
    const dialogDescription = src.match(/<Dialog\.Description[^>]*>([\s\S]*?)<\/Dialog\.Description>/)?.[1] ?? "";
    if (!/effectKind=\{act\.effectKind\}/.test(src)) {
      fail("DecideQueue.tsx does not pass the server-derived effect kind into EnumerableApply");
    } else if (!/confirmationLead:\s*"Plans the tracked step for"/.test(src)) {
      fail("DecideQueue.tsx still describes tracked planning as a generic apply");
    } else if (!/const isSingleton = representedCount === 1 && members\.length === 1/.test(src) || !/isSingleton\s*\n\s*\? label/.test(src)) {
      fail("DecideQueue.tsx does not use the owner action label for a singleton trigger");
    } else if (!/isSingleton && policyNote\s*\n\s*\? policyNote/.test(src)) {
      fail("DecideQueue.tsx does not surface the singleton policy boundary before confirmation");
    } else if (!/isSingleton/.test(dialogDescription) || !/\{label\}/.test(dialogDescription) || !/singletonMember\?\.label/.test(dialogDescription)) {
      fail("DecideQueue.tsx singleton dialog does not name the owner action and target");
    } else if (!/policyNote/.test(dialogDescription)) {
      fail("DecideQueue.tsx policy boundary is outside Dialog.Description and will not be announced with the confirmation");
    } else if (!/isSingleton\s*\n\s*\? `Confirm \$\{label\}`/.test(src)) {
      fail("DecideQueue.tsx singleton confirmation control still has a count-only accessible name");
    } else {
      pass("DecideQueue.tsx uses owner-specific singleton copy and announces the owner policy boundary in the confirmation dialog");
    }
  }

  /* ---------- f2: missing-owner naming (not the bare label) ---------- */

  // (f) The pure owner-unavailable message names the specific area and the
  // honest state, and is never literally the old bare label.
  {
    const msg = ownerUnavailableMessage("route-promotions");
    if (msg === "Owner action unavailable") {
      fail("ownerUnavailableMessage returned the bare legacy label instead of a named line");
    } else if (!/route promotions/.test(msg)) {
      fail(`ownerUnavailableMessage does not name the area, got ${JSON.stringify(msg)}`);
    } else if (!/park\/dismiss/i.test(msg)) {
      fail(`ownerUnavailableMessage does not say what the operator can still do, got ${JSON.stringify(msg)}`);
    } else {
      pass(`ownerUnavailableMessage("route-promotions") names the owner and the honest state: ${JSON.stringify(msg)}`);
    }
  }

  // (g) Two different areas must produce two different messages — proves
  // the text is actually derived from the group's area, not a second
  // hardcoded generic string standing in for the first.
  {
    const a = ownerUnavailableMessage("route-promotions");
    const b = ownerUnavailableMessage("synthesis-contradictions");
    if (a === b) fail("ownerUnavailableMessage does not vary by area — looks like a second hardcoded generic string");
    else pass("ownerUnavailableMessage varies by area (derived from the read model, not a second generic constant)");
  }

  // (h) DecideQueue.tsx must actually call ownerUnavailableMessage at both
  // render sites that used to show the bare label: the CapabilityStatus
  // badge's tooltip detail, and the DecisionDetail copy-mode card body. And
  // the old generic, non-naming paragraph must be gone from the card body.
  {
    const src = read("components/decide/DecideQueue.tsx");
    const callSites = src.match(/ownerUnavailableMessage\(/g)?.length ?? 0;
    if (callSites < 2) {
      fail(`expected ownerUnavailableMessage(...) wired at both the badge tooltip and the card body, found ${callSites} call site(s)`);
    } else {
      pass(`ownerUnavailableMessage(...) is wired at ${callSites} render sites in DecideQueue.tsx`);
    }
    if (/Primary action unavailable/.test(src)) {
      fail("DecideQueue.tsx still renders the old generic 'Primary action unavailable' heading instead of the named-owner line");
    } else {
      pass("the old generic 'Primary action unavailable' heading is gone — replaced by the named-owner line");
    }
  }

  /* ---------- f3: final confirmation shows the submitted values ---------- */

  // The final GovernedAct review must use the shared ConfirmDialog and its
  // extra-content slot to show every field collected for submission. The
  // value itself is rendered verbatim, not merely the field label or a count.
  // This is a source-wiring falsifier because the console test harness has no
  // DOM renderer; importing the component would also pull in Next client code.
  {
    const src = read("components/decide/GovernedAct.tsx");
    const dialog = src.match(/<ConfirmDialog[\s\S]*?<\/ConfirmDialog>/)?.[0] ?? "";
    if (!/<ConfirmDialog[\s\S]*onConfirm=\{\(\) => void submit\("confirm"/.test(dialog)) {
      fail("GovernedAct final review is not wired through the shared ConfirmDialog");
    } else {
      pass("GovernedAct final review is wired through the shared ConfirmDialog");
    }
    if (!/const confirmationFields = Object\.entries\(confirmationFieldValues\)/.test(src)) {
      fail("GovernedAct does not render from a stable submission-field snapshot");
    } else if (!/setConfirmationFieldValues\(fieldValues\)/.test(src)) {
      fail("GovernedAct does not capture the submission-field snapshot before opening confirmation");
    } else if (!/confirmationFields\.map\(\(\[name, value\]\)/.test(src)) {
      fail("GovernedAct does not render every collected submission field in the final confirmation");
    } else if (!/<code[^>]*>\{value\}<\/code>/.test(src)) {
      fail("GovernedAct final confirmation does not render each collected value verbatim");
    } else {
      pass("GovernedAct final confirmation renders every collected field value verbatim");
    }
    if (!/submit\("confirm", Object\.fromEntries\(confirmationFields\)\)/.test(dialog)) {
      fail("GovernedAct confirm request is not bound to the exact field snapshot shown in the dialog");
    } else {
      pass("GovernedAct confirm request is bound to the exact field snapshot shown in the dialog");
    }
    if (!/previewResult/.test(dialog) || !/act\.policyNote/.test(dialog) || !/act\.argv/.test(dialog)) {
      fail("GovernedAct lost preview, policy, or argv details while adopting ConfirmDialog");
    } else {
      pass("GovernedAct preserves preview, policy, and argv details inside ConfirmDialog");
    }
  }

  /* ---------- f4: object-first face + readiness hierarchy ---------- */

  {
    const src = read("components/decide/DecideQueue.tsx");
    if (!/decideReadinessSections\(visible\)/.test(src)) {
      fail("DecideQueue does not group the visible queue from the server-owned face state");
    } else if (!/Act now/.test(src) || !/Needs context/.test(src)) {
      fail("DecideQueue is missing the two explicit readiness group labels");
    } else {
      pass("DecideQueue groups the queue into Act now and Needs context from the face state");
    }

    for (const tab of ["Summary", "Action", "History", "Evidence"]) {
      if (!new RegExp(`value=["']${tab.toLowerCase()}["'][^>]*>${tab}<`).test(src)) {
        fail(`Decision face is missing its ${tab} tab`);
      }
    }
    if (!/StateBadge state=\{group\.face\.state\} label=\{group\.face\.stateLabel\}/.test(src)) {
      fail("Decision face does not render the shared state vocabulary from group.face");
    } else if (!/Owner<\/span>[\s\S]*group\.face\.owner/.test(src)) {
      fail("Decision face does not expose the human owner before technical evidence");
    } else if (!/FreshnessChip[\s\S]*group\.face\.freshness\.generatedAt/.test(src)) {
      fail("Decision face does not expose projection freshness in its primary header");
    } else {
      pass("Decision face exposes state, owner, and freshness before secondary records");
    }
  }

  /* ---------- f5: routing preserves exact owner authority ---------- */

  // Routing mounts the same GovernedAct as Decide, so it must also show the
  // complete Go-owned AuthorityFace before execution and inside any
  // confirmation that the owner action already requires. This is a wiring
  // falsifier: removing the shared face from Routing or the dialog must red
  // even if the server-side authority resolver remains green.
  {
    const authorityComponentPath = path.join(SRC_ROOT, "components/authority/AuthorityFaceDetails.tsx");
    const authorityComponent = fs.existsSync(authorityComponentPath)
      ? fs.readFileSync(authorityComponentPath, "utf8")
      : "";
    const authorityModel = read("lib/action-authority.ts");
    const decide = read("components/decide/DecideQueue.tsx");
    const routing = read("components/routing/PromotionBand.tsx");
    const governed = read("components/decide/GovernedAct.tsx");

    if (!authorityComponent || !/export function AuthorityFaceDetails/.test(authorityComponent)) {
      fail("the reusable client-safe AuthorityFaceDetails renderer is missing");
    } else if (!/authorityDisplay\(authority\)/.test(authorityComponent)) {
      fail("AuthorityFaceDetails does not consume the shared owner-display projection");
    } else if (!/export function authorityDisplay/.test(authorityModel) || !/authorityDisplay\(authority\)/.test(authorityModel)) {
      fail("authority summary/details are not derived from one shared display projection");
    } else {
      pass("one client-safe AuthorityFace renderer owns the exact details/summary projection");
    }

    if (!/import \{ AuthorityFaceDetails \} from "@\/components\/authority\/AuthorityFaceDetails"/.test(decide) ||
        /function AuthorityFaceDetails\(/.test(decide)) {
      fail("DecideQueue still owns a duplicate AuthorityFace renderer instead of the shared component");
    } else {
      pass("DecideQueue reuses the shared AuthorityFace renderer");
    }

    const routingFace = routing.indexOf("<AuthorityFaceDetails authority={group.face.authority}");
    const routingAct = routing.indexOf("<GovernedAct");
    if (!/import \{ AuthorityFaceDetails \} from "@\/components\/authority\/AuthorityFaceDetails"/.test(routing) ||
        routingFace < 0 || routingAct < 0 || routingFace > routingAct ||
        !/authority=\{group\.face\.authority\}/.test(routing.slice(routingAct))) {
      fail("Routing does not show and pass the exact group.face.authority before its governed act");
    } else {
      pass("Routing shows the exact owner AuthorityFace before execution and passes it to GovernedAct");
    }

    const dialog = governed.match(/<ConfirmDialog[\s\S]*?<\/ConfirmDialog>/)?.[0] ?? "";
    if (!/authority\?: AuthorityFace/.test(governed) ||
        !/<AuthorityFaceDetails authority=\{authority\}/.test(dialog) ||
        !/body=\{authority\?\.wording \?\? confirmationDescription/.test(dialog)) {
      fail("GovernedAct confirmation does not show the full AuthorityFace with exact owner wording");
    } else {
      pass("GovernedAct confirmation reuses the full AuthorityFace and exact owner wording");
    }
  }

  /* ---------- f6: long confirmation stays reachable ---------- */

  // AuthorityFace adds enough real content to overflow a phone or short
  // window. The shared dialog therefore owns a structural layout contract:
  // viewport-bounded shell, one contained scroll region for all descriptive
  // content, and a non-scrolling action row. A max-height on the wrong node
  // or scrolling the buttons with the content does not satisfy this guard.
  {
    const src = read("components/kit/ConfirmDialog.tsx");
    const content = src.match(/<Dialog\.Content[\s\S]*?<\/Dialog\.Content>/)?.[0] ?? "";
    const scrollRegion = content.match(/<div\s+className="min-h-0 flex-1 overflow-y-auto overscroll-contain[^\"]*"[^>]*>([\s\S]*?)<\/div>\s*<div className="mt-4 flex shrink-0/)?.[1] ?? "";

    if (!/max-h-\[calc\(100dvh-2rem\)\]/.test(content) ||
        !/w-\[calc\(100vw-2rem\)\]/.test(content) ||
        !/flex[^\"]*flex-col[^\"]*overflow-hidden/.test(content)) {
      fail("ConfirmDialog shell is not bounded to narrow/short viewports with contained overflow");
    } else if (!scrollRegion ||
        !/Dialog\.Description/.test(scrollRegion) ||
        !/policyNote/.test(scrollRegion) ||
        !/argv/.test(scrollRegion) ||
        !/children/.test(scrollRegion)) {
      fail("ConfirmDialog does not keep all long descriptive/authority content inside its scroll region");
    } else if (!/className="mt-4 flex shrink-0[^\"]*border-t[^\"]*pt-4"/.test(content)) {
      fail("ConfirmDialog actions are not pinned outside the scroll region");
    } else {
      pass("ConfirmDialog is viewport-bounded; long authority content scrolls while actions remain reachable");
    }
  }

  /* ---------- f7: partial enumerable scope stays truthful ---------- */

  // The model may expose only independently authorized B2 members, while the
  // card still represents a larger group. The action UI must keep those two
  // counts separate and say explicitly that unavailable members do not move.
  {
    const src = read("components/decide/DecideQueue.tsx");
    if (!/representedCount:\s*number/.test(src) ||
        !/representedCount=\{act\.representedCount\}/.test(src)) {
      fail("EnumerableApply does not receive the full represented group count");
    } else if (!/const scopeLabel = `\$\{members\.length\} ready of \$\{representedCount\}`/.test(src) ||
        !/const unchangedLabel = unavailableCount > 0[\s\S]*unavailable[\s\S]*remain unchanged/.test(src)) {
      fail("EnumerableApply does not derive exact ready/total and unavailable/unchanged copy");
    } else if (!/aria-label="Executable member scope"[\s\S]*\{scopeLabel\}[\s\S]*\{unchangedLabel\}/.test(src)) {
      fail("the mixed-member scope truth is not rendered before the action");
    } else {
      pass("partial enumerable UI shows exact ready/total scope and keeps unavailable members unchanged");
    }
  }

  /* ---------- f8: card grammar — WHAT/consequence/action lead the card, one
   * systemic-blocker banner instead of N identical stamps, jargon reasons
   * mapped to plain language (work_58a058a3d028) ---------- */

  // f8a: blockedReasonPlain maps known compound-internal jargon reasons to
  // one plain sentence with a next step, carries a dynamic token through,
  // and passes an unknown (e.g. backend-owned) reason through unchanged —
  // it must never invent a mapping it doesn't actually have.
  {
    const known = blockedReasonPlain("No complete current owner authority bundle is available.");
    const dynamic = blockedReasonPlain("Owner authority evidence is stale; execution is unavailable.");
    const unknown = blockedReasonPlain("Some future backend-owned sentence nobody mapped yet.");
    if (!/approve\/reject action/i.test(known)) {
      fail(`blockedReasonPlain does not map the known "no authority bundle" reason to plain language: ${JSON.stringify(known)}`);
    } else if (!/live status feed is stale/i.test(dynamic)) {
      fail(`blockedReasonPlain does not carry the dynamic token into its plain-language template: ${JSON.stringify(dynamic)}`);
    } else if (unknown !== "Some future backend-owned sentence nobody mapped yet.") {
      fail(`blockedReasonPlain must pass an unknown reason through unchanged, got ${JSON.stringify(unknown)}`);
    } else {
      pass("blockedReasonPlain maps known jargon reasons to plain language and passes unknown reasons through unchanged");
    }
  }

  // f8b: queueSystemicBlocker collapses N identical blocked reasons into one
  // queue-level fact, stays silent on mixed reasons or a healthy queue, and
  // prefers a feature-detected projection reason when one is supplied.
  {
    const face = (state: string, unavailableReason: string) => ({ face: { state: state as never, unavailableReason } });
    const allBlocked = [face("waiting", "same reason"), face("waiting", "same reason"), face("needs_attention", "same reason")];
    const banner = queueSystemicBlocker(allBlocked);
    const mixed = queueSystemicBlocker([face("waiting", "reason A"), face("waiting", "reason B")]);
    const healthy = queueSystemicBlocker([face("ready", ""), face("ready", "")]);
    const projected = queueSystemicBlocker([face("ready", ""), face("waiting", "same reason")], "queue-level reason");
    if (!banner || banner.reason !== "same reason" || banner.blockedCount !== 3 || banner.totalCount !== 3) {
      fail(`queueSystemicBlocker did not collapse identical reasons into one banner: ${JSON.stringify(banner)}`);
    } else if (mixed !== null) {
      fail(`queueSystemicBlocker must stay silent when blocked groups disagree on the reason, got ${JSON.stringify(mixed)}`);
    } else if (healthy !== null) {
      fail(`queueSystemicBlocker must stay silent when the queue is decidable, got ${JSON.stringify(healthy)}`);
    } else if (!projected || projected.reason !== "queue-level reason") {
      fail(`queueSystemicBlocker must prefer a feature-detected projection reason, got ${JSON.stringify(projected)}`);
    } else {
      pass("queueSystemicBlocker collapses identical-reason queues into one banner, stays silent otherwise, and prefers a feature-detected reason");
    }
  }

  // f8c: source-wiring — the card actually leads with the quoted object text
  // outside the tabs, the queue renders the single decidability banner, the
  // per-row stamp is demoted when it fires, and no render site shows the raw
  // unavailableReason verbatim (every site routes through blockedReasonPlain).
  {
    const src = read("components/decide/DecideQueue.tsx");
    if (!/<blockquote[^>]*>\s*\{group\.objectText/.test(src)) {
      fail("the card no longer leads with the quoted objectText outside the tabs");
    } else if (!/systemicBlocker\.totalCount - systemicBlocker\.blockedCount/.test(src)) {
      fail("the queue no longer renders a single '<N> of <M> decidable' banner");
    } else if (!/demoteBlockedStamp/.test(src)) {
      fail("the per-row stamp is no longer demoted when a systemic-blocker banner covers it");
    } else if (/\{group\.face\.unavailableReason\}/.test(src)) {
      fail("a raw unavailableReason is rendered verbatim somewhere instead of through blockedReasonPlain(...)");
    } else if ((src.match(/blockedReasonPlain\(/g) ?? []).length < 4) {
      fail("blockedReasonPlain is not wired at every card/row site that used to show the raw reason string");
    } else {
      pass("the Decide card leads with the quoted object text, shows one banner for a systemic blocker, demotes the per-row stamp, and never renders a raw reason string verbatim");
    }
  }

  /* ---------- f9: Panel proof continuity is wired without authority theater ---------- */

  {
    const queue = read("components/decide/DecideQueue.tsx");
    const panel = read("components/decide/PanelProofContinuity.tsx");
    if (
      !/import \{ PanelProofContinuity \}/.test(queue) ||
      !/if \(selected\?\.panel\)/.test(queue) ||
      !/<PanelProofContinuity/.test(queue)
    ) {
      fail("DecideQueue does not route a selected Panel group into the proof-continuity treatment");
    } else if (!/Panel verdict is evidence, not authority\./.test(panel)) {
      fail("Panel proof continuity lost the evidence-not-authority boundary");
    } else if (
      ![
        "Observed candidate",
        "Lens review",
        "Judge synthesis",
        "Human review",
        "Governed action",
        "PolicyDecision",
        "Receipt",
        "Transition",
      ].every((label) => panel.includes(label))
    ) {
      fail("Panel proof continuity no longer exposes all eight evidence-to-proof stages");
    } else if (!/review\.choices\.map/.test(panel) || !/setSelectedChoice\(choice\.id\)/.test(panel)) {
      fail("Panel operator choices are no longer rendered from the owner-projected decision card");
    } else if (!/<Button[\s\S]*disabled[\s\S]*Apply \$\{selected\.label\}/.test(panel)) {
      fail("Panel combined adjudication no longer fails closed while its safe console adapter is absent");
    } else if (!/safeRecordAct && safeRecordAuthority/.test(panel) || !/<GovernedAct/.test(panel)) {
      fail("Panel review recording is not gated on current authority through the existing governed action surface");
    } else if (/fetch\(/.test(panel)) {
      fail("Panel proof-continuity component bypasses GovernedAct with its own network mutation");
    } else if (!/<details className="group border-t border-hair bg-soft">/.test(panel)) {
      fail("Panel technical depth is no longer collapsed by default");
    } else {
      pass("Panel proof continuity keeps one claim primary, exposes all stages and choices, fails closed on the unwired combined act, and reuses governed review recording only when current");
    }
  }

  return { ok, details };
}
