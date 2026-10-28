# Salvage audit — slice/prediction-mechanism-20260719

- Work envelope: `work_891752d1bd00` (owner `stalled-lane-salvage`, state `claimed`)
- Audit date: 2026-08-13 · Auditor lane: `claude_qwen` (delegate burst `burst-prediction-20260813`)
- Audit branch: `salvage/prediction-mechanism-audit-20260813` (this commit; not pushed)
- `origin/main` read this session: `19ef1562` (fetched in the same turn)
- Envelope delivery base: `967a87ce` (ancestor of current main; main is 1232 commits ahead)
- Success predicate of the envelope: branch reaches **LANDED on origin/main** or **PARKED
  with content-verified evidence and a terminal lease**. Publication mode is `required`,
  and this session carries a **no-push** fence — LANDED is unreachable this session by
  construction; the predicate resolves to PARKED unless content is already on origin/main.

## Canonical report block

```
LANE:        slice/prediction-mechanism-20260719
OUTCOME:     PARKED
WHAT IT WAS: Three commits ahead of a now-rewritten base: (1) a work-prediction
             "mechanism" dimension — stake mechanism risk at create, answer it
             (held|diverged) at close, diff it at surprise — (2) corpus query
             adjacent-chunk neighbor expansion, (3) statussurface infographic
             rendering. Commits (2) and (3) are content-verified SUPERSEDED on
             origin/main by other implementations; commit (1) is content-verified
             ABSENT from origin/main and is the only residue.
EVIDENCE:    cherry 423/426 patch-landed, 3 unlanded (95f64551, 2740b45e, 16ccdbae)
             · per-commit content verification vs origin/main@19ef1562 (below)
             · base battery: build + 5 affected internal packages + scoped
               cmd/compound status/prediction tests ALL GREEN at 967a87ce
             · replay of each commit onto 967a87ce: 3/3 CONFLICT (below)
             · no push performed
PARK REASON: Two of three commits superseded by different landed implementations;
             the residue (prediction mechanism) does not apply cleanly to the
             delivery base (4 files conflict), and resolving those conflicts is a
             re-authoring decision the envelope classifies audit_only. "If two
             readings are possible and one of them is destructive, PARK."
LEASE:       work_891752d1bd00 (claimed; terminal lease to be recorded by the
             orchestrator at close — executor session holds no compound-CLI authority)
FOLLOW-UP:   If the mechanism dimension is still wanted: re-author 16ccdbae's
             semantics (WorkPrediction.Mechanism stake, CloseInput.MechanismOutcome
             answer, SurpriseSignal.MechanismResolution diff, work_surprise_unresolved
             predicate) as a fresh single-commit slice off current origin/main.
             That is a new-envelope decision, not an inheritance from this branch.
```

## 1 · Patch-id census (fresh, this session)

`git cherry origin/main slice/prediction-mechanism-20260719` at origin/main `19ef1562`:

- 423 commits patch-equivalent to main (`-`), 3 unlanded (`+`):
  - `95f64551` feat(statussurface): infographic status rendering with absolute token counts
  - `2740b45e` feat(corpus): expand query hits with gated adjacent-chunk context
  - `16ccdbae` feat(envelope): stake and diff a mechanism dimension on work predictions

The ledger (2026-07-27) recorded exactly this shape (3 unlanded, CONFLICT class), and the
2026-07-30 continuation session PARKED the lane after verifying 95f64551 superseded and the
other two then-fresh. The envelope's prediction ("Two commits are superseded; verify
mechanism prediction residue against current main before rebase") is CONFIRMED for the first
half: 2740b45e has SINCE been superseded too (section 2.2). Its second half is answered in
2.3: the mechanism residue is absent from current main.

## 2 · Content verification, per commit (never by commit message)

### 2.1 — 95f64551 statussurface → SUPERSEDED

Main carries `798ca059` (same subject, 2026-07-20) plus the next-day fix `668ae016`
"fix(statussurface): derive context usage from token counts". Content tell: the lane's
tests assert REMAINING-percentage capacity (`5h 63% / wk 24%` in
`cmd/compound/system_status_surface_test.go`), while main's tests assert
CONSUMED-percentage (`5h 37% / wk 76%`) — 37 = 100−63, 76 = 100−24. Main's post-fix
semantics won; the lane carries the pre-fix original. Landing the lane commit would
re-introduce tests that fail against the shipped production semantics. (Same finding as
the 2026-07-30 session against then-main `0674917e`; re-verified against `19ef1562`.)

### 2.2 — 2740b45e corpus neighbor expansion → SUPERSEDED

Main carries `e9ec22bf` "feat(corpus): add --neighbors radius for query-hit context map"
(2026-07-30, eleven days after the lane commit) plus later evolution. Same user-visible
feature and invariants, different implementation:

- same shape: `QueryNeighbor` with deliberately NO score field, `MaxNeighborRadius = 5`,
  `QueryOptions.NeighborRadius` with negative-rejected-not-clamped validation,
  `--neighbors R` on `compound memory search`, dropped-not-degraded gating of withheld
  neighbors — all present on main (`internal/corpus/query.go`).
- diverged design: the lane threaded `Gate`/`Allowlist` through `QueryOptions`; main
  replaced that with a preflight seam (`runCorpusEngineQuery`) and added source-observation
  wiring, and main's tree further carries `query_neighbor_test.go` (AA-conflict class in
  replay — both sides authored it independently).

Patch-id blind spot confirmed exactly as the ledger method warns: intent landed by another
route reports as unlanded; content comparison is what settles it.

### 2.3 — 16ccdbae prediction mechanism → NOT LANDED (the residue)

Verified ABSENT from `origin/main@19ef1562` by symbol sweep, not message search:

- `MechanismOutcome` — zero hits anywhere on main.
- `WorkPrediction` on main (`internal/envelope/envelope.go:920-929`) has exactly
  `Outcome`, `MoveType`, `Note` — no `Mechanism` stake field.
- `internal/mem/surprise.go` on main diffs exactly two dimensions (`PredictedOutcome` +
  `PredictedMoveType`, lines 109-115); no mechanism comparison, no `unresolved`
  resolution, no `work_surprise_unresolved` predicate.
- The only `mechanism` surface on main is the CLOSE-time receipt annotation
  (`FailureCause.Mechanism` / `FailureMechanism` in `internal/work/work.go`) — which the
  lane's own commit message names as pre-existing ("already recordable at close … but
  un-stakeable at create and never diffed"). The asymmetry the lane repairs is still open.

## 3 · BASE tested before blaming the branch

Envelope-bound delivery base `967a87ce` was tested in an isolated detached worktree
(never the main checkout):

- `go build ./...` — exit 0.
- `go test -count=1` on every package the lane touches:
  `internal/envelope` ok 0.3s · `internal/mem` ok 30.0s · `internal/work` ok 380.5s ·
  `internal/corpus` ok 7.2s · `internal/statussurface` ok 0.7s.
- Scoped `cmd/compound` runs: all 10 status-surface tests PASS; all 10 prediction/close
  tests PASS (incl. `TestWorkCloseCLIMintsSurpriseCandidateOnDivergence`,
  `TestWorkCloseCLIMatchingPredictionMintsNothing`).

The base is GREEN everywhere the lane reaches. Nothing observed in this audit may be
attributed to base instability, and no battery red was inherited or produced. If a future
rebase attempt shows failures, they are not base failures; the contention-escape rule
(identical failure on base ⇒ machine contention) was pre-empted by a green base.

## 4 · Replay of each lane commit onto the delivery base (3/3 CONFLICT)

Each commit cherry-picked in isolation onto `967a87ce` (scratch branches, aborted after
recording, worktree restored):

| commit | result | conflicting files |
|---|---|---|
| `95f64551` | CONFLICT | `internal/statussurface/statussurface.go` |
| `2740b45e` | CONFLICT | `cmd/compound/corpus.go`, `internal/corpus/chunk.go`, `internal/corpus/query.go`, `internal/corpus/query_neighbor_test.go` (AA) |
| `16ccdbae` | CONFLICT | `cmd/compound/work_cli.go`, `internal/envelope/envelope.go`, `internal/envelope/envelope_test.go`, `internal/work/work_test.go` |

The two superseded commits conflict against their superseder implementations — resolving
those conflicts means either re-introducing superseded code or deleting the lane's own
changes, i.e. nothing to land. The residue (`16ccdbae`) conflicts because 1232 commits of
envelope/work evolution moved the touched structures — resolvable only by re-authoring,
which is a product/scope decision (audit_only envelope), not salvage.

## 5 · Lane tip tested too — exact attribution of the one red test

The lane tip (`16ccdbae`, isolated worktree, `GOFLAGS=-p=2` under machine load) ran the
same surface set:

- `go build ./...` — exit 0.
- `internal/envelope` ok · `internal/mem` ok · `internal/work` ok · `internal/corpus` ok ·
  `internal/statussurface` ok — ALL FIVE GREEN at the lane tip.
- All four mechanism falsifier tests PASS at the tip
  (`TestWorkCloseCLIMechanismDivergenceMintsOnMatchingOutcomeAndMoveType`,
  `TestWorkCloseCLIUnansweredMechanismSurfacesAndMints`,
  `TestWorkCloseCLIUnstakedMechanismIsInert`, `TestWorkCloseCLIHeldMechanismIsQuiet`).
- Scoped `cmd/compound` run: EXACTLY ONE red —
  `TestSupplementLiveClaudeCapacityProbesOnlyWhenOwnedSourcesAreEmpty`, rendering
  `5h 37% │ wk 76%` against remaining-percent assertions `5h 63% / wk 24%`.

Attribution of that one red (verified, not guessed):

- The assertion lines are UNTOUCHED by the lane commit — they exist verbatim at `95f64551^`
  (old-base test written against the retired `left:` remaining-percent format).
- `95f64551` converted the rich themes to CONSUMED-percentage rendering (its own commit
  message says so) and converted two of the three affected tests, leaving this one
  asserting remaining-percentage against its own new consumed-percentage renderer. The
  lane tip is therefore red on the superseded commit's OWN surface — the commit was
  authored red there, independent of any rebase.
- Main's lineage fixed exactly this: `798ca059` + `668ae016` (same day) carry the
  consumed-percentage semantics, and current main's version of this test asserts exactly
  the `5h 37% / wk 76%` the lane-tip renderer emits — green on main.
- The red is NOT machine contention: the delivery base is green on the identical surface
  (section 3) and the failure is a deterministic semantic mismatch, not a wall-clock flake.
- The red is NOT the mechanism commit's, and NOT the corpus commit's: every package and
  test they own is green at the tip.

## 6 · Attribution summary (why nothing is blamed without proof)

- Delivery base `967a87ce`: fully green (section 3). Not blamed.
- Current main: green on all the same surfaces. Not blamed.
- Lane tip: green except one test, and that test is pinned to the superseded
  statussurface commit `95f64551` (section 5). The residue commit `16ccdbae` and the
  superseded corpus commit `2740b45e` are green on their own surfaces at the tip.
- Therefore the PARK is not a blame of the branch as a whole and not a blame of the
  residue. It is the canonical outcome for a lane whose content is partly superseded,
  whose superseded part carries a red it was authored with, and whose residue needs a
  re-authoring decision the envelope classifies as audit_only.

## 7 · Ceremony posture

- Worktree + named branch created from `origin/main@19ef1562`; this REPORT.md is committed
  on `salvage/prediction-mechanism-audit-20260813`. NOT pushed; no force; no history
  rewrite; no branch deletion; no worktree removal; no gate touched.
- No `commit:` artifact is claimed (nothing landed). Close must carry `evidence:`-class
  proof refs only, per the bridge/audit close law and the 2026-07-30 measured mechanics
  (`cancelled` terminal via evidence refs when nothing lands).
- Terminal lease for `work_891752d1bd00` is to be recorded by the orchestrator when it
  closes the envelope; the executor session holds no compound-CLI authority (policy floor).

## Appendix — evidence commands (reproducible)

```bash
git fetch origin main && git rev-parse origin/main                 # 19ef1562...
git cherry origin/main slice/prediction-mechanism-20260719 | sort | uniq -c -w1
git log --oneline -S MaxNeighborRadius main -- internal/corpus/    # e9ec22bf
git show main:internal/envelope/envelope.go | sed -n '920,930p'    # WorkPrediction, no Mechanism
git grep -n MechanismOutcome main                                  # zero hits
git show main:internal/mem/surprise.go | sed -n '105,120p'         # two-dimension diff only
git show 95f64551:cmd/compound/system_status_surface_test.go | grep -n '5h 63'
git show main:cmd/compound/system_status_surface_test.go  | grep -n '5h 37'
git worktree add --detach <wt> 967a87ce5009cbd6d53bfd7d5f44757d04806e42
cd <wt> && GOFLAGS=-p=4 go build ./... && GOFLAGS=-p=4 go test -count=1 \
  ./internal/envelope/ ./internal/mem/ ./internal/work/ ./internal/corpus/ ./internal/statussurface/
for c in 95f64551 2740b45e 16ccdbae; do git cherry-pick -x $c; git status --porcelain; git cherry-pick --abort; done
```
