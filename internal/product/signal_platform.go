package product

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/work"

	"gopkg.in/yaml.v3"
)

// A platform_change signal is an external fact about a platform surface (a new
// device form factor, an SDK requirement, a store rule) with a date and its
// sources. Unlike a metric signal it names no product: intake finds every
// product on the surface and turns the signal into, per product, one proposed
// Form and one proposed work plan, plus one shared work plan for the machine
// prerequisites. Nothing executes; routing admits and assigns the work.
const (
	PlatformChangeKind          = "platform_change"
	PlatformIntakeSchemaVersion = "compound.product_platform_intake.v0.1"

	platformIntakeActor = "product-signal-intake"
	platformWorkKind    = "product_platform_signal"
	platformContractRef = "product_platform_contract:"
	platformSignalRef   = "product_platform_signal:"
)

var platformDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// PlatformChangeDocument is a `kind: platform_change` signal document.
type PlatformChangeDocument struct {
	SchemaVersion string            `json:"schema_version" yaml:"schema_version"`
	Kind          string            `json:"kind" yaml:"kind"`
	ID            string            `json:"id" yaml:"id"`
	Title         string            `json:"title" yaml:"title"`
	Surface       string            `json:"surface" yaml:"surface"`
	EffectiveDate string            `json:"effective_date" yaml:"effective_date"`
	Sources       []PlatformSource  `json:"sources" yaml:"sources"`
	Facts         []string          `json:"facts" yaml:"facts"`
	FormDelta     PlatformFormDelta `json:"form_delta" yaml:"form_delta"`
	Prerequisites []string          `json:"prerequisites,omitempty" yaml:"prerequisites,omitempty"`
}

// PlatformSource is one cited source: where the claim was read.
type PlatformSource struct {
	URL   string `json:"url" yaml:"url"`
	Claim string `json:"claim" yaml:"claim"`
}

// PlatformFormDelta is the Form the signal implies for every product on its
// surface: the stage it changes, the goal, the parameters, and the acceptance
// lines a QA lane can check.
type PlatformFormDelta struct {
	Stage      string            `json:"stage" yaml:"stage"`
	Goal       string            `json:"goal" yaml:"goal"`
	Parameters map[string]string `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	Acceptance []string          `json:"acceptance" yaml:"acceptance"`
}

// PlatformIntakeReport is the intake plan (dry run) or what intake recorded.
type PlatformIntakeReport struct {
	SchemaVersion string                  `json:"schema_version"`
	Mode          string                  `json:"mode"`
	SignalID      string                  `json:"signal_id"`
	Title         string                  `json:"title"`
	Surface       string                  `json:"surface"`
	EffectiveDate string                  `json:"effective_date"`
	SourcePath    string                  `json:"source_path,omitempty"`
	EvidenceID    string                  `json:"evidence_id,omitempty"`
	Matched       []PlatformIntakeProduct `json:"matched"`
	Skipped       []PlatformIntakeSkip    `json:"skipped"`
	Prerequisite  *PlatformWorkItem       `json:"prerequisite,omitempty"`
	Counts        PlatformIntakeCounts    `json:"counts"`
}

// PlatformIntakeProduct is one product on the signal's surface.
type PlatformIntakeProduct struct {
	ProductID   string           `json:"product_id"`
	MatchReason string           `json:"match_reason"`
	Form        Form             `json:"form"`
	FormStatus  string           `json:"form_status"`
	Recipe      string           `json:"recipe"`
	Work        PlatformWorkItem `json:"work"`
}

// PlatformIntakeSkip is a product the signal does not reach, and why.
type PlatformIntakeSkip struct {
	ProductID string `json:"product_id"`
	Reason    string `json:"reason"`
}

// PlatformWorkItem is one proposed work plan: the work envelope fields and the
// action context (the task contract) it carries.
type PlatformWorkItem struct {
	WorkID           string                 `json:"work_id"`
	Owner            string                 `json:"owner"`
	State            string                 `json:"state"`
	Status           string                 `json:"status"`
	ContractRef      string                 `json:"contract_ref"`
	SuccessPredicate string                 `json:"success_predicate"`
	DesiredOutcome   string                 `json:"desired_outcome"`
	NonGoals         []string               `json:"non_goals"`
	EvidenceRefs     []string               `json:"evidence_refs"`
	ActionContext    work.WorkActionContext `json:"action_context"`
	PlanID           string                 `json:"plan_id,omitempty"`
	PlanPath         string                 `json:"plan_path,omitempty"`
	Policy           string                 `json:"policy,omitempty"`
	PriorPlanIDs     []string               `json:"prior_plan_ids,omitempty"`
	Note             string                 `json:"note,omitempty"`
}

// PlatformIntakeCounts totals the forms and work plans (the prerequisite plan
// included) by what intake did, or would do, with them.
type PlatformIntakeCounts struct {
	Matched         int `json:"matched"`
	Skipped         int `json:"skipped"`
	FormsNew        int `json:"forms_new"`
	FormsSuperseded int `json:"forms_superseded"`
	FormsUnchanged  int `json:"forms_unchanged"`
	WorkNew         int `json:"work_new"`
	WorkReplanned   int `json:"work_replanned"`
	WorkUnchanged   int `json:"work_unchanged"`
}

// LoadPlatformChange reads path as a platform_change signal document. ok is
// false when the file is not one (unreadable, or another signal kind), so the
// caller can hand it to the metric-signal path unchanged.
func LoadPlatformChange(path string) (PlatformChangeDocument, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return PlatformChangeDocument{}, false, nil
	}
	var header struct {
		Kind string `yaml:"kind"`
	}
	if yaml.Unmarshal(b, &header) != nil || strings.TrimSpace(header.Kind) != PlatformChangeKind {
		return PlatformChangeDocument{}, false, nil
	}
	var doc PlatformChangeDocument
	if err := decodeKnownYAML(b, &doc); err != nil {
		return doc, true, fmt.Errorf("platform_change %s: %w", path, err)
	}
	if err := ValidatePlatformChange(doc); err != nil {
		return doc, true, fmt.Errorf("platform_change %s: %w", path, err)
	}
	return doc, true, nil
}

// ValidatePlatformChange refuses a document intake could not key or cite. The
// id becomes a form surface key, so it shares the form key alphabet.
func ValidatePlatformChange(doc PlatformChangeDocument) error {
	if doc.SchemaVersion != SignalSchemaVersion || doc.Kind != PlatformChangeKind {
		return fmt.Errorf("schema_version must be %s with kind %s", SignalSchemaVersion, PlatformChangeKind)
	}
	if !formKey.MatchString(doc.ID) {
		return fmt.Errorf("id %q must match %s (it keys the proposed form)", doc.ID, formKey)
	}
	if !formKey.MatchString(doc.Surface) {
		return fmt.Errorf("surface %q must match %s", doc.Surface, formKey)
	}
	if strings.TrimSpace(doc.Title) == "" {
		return fmt.Errorf("title required")
	}
	if _, err := time.Parse("2006-01-02", doc.EffectiveDate); err != nil || !platformDate.MatchString(doc.EffectiveDate) {
		return fmt.Errorf("effective_date %q must be YYYY-MM-DD", doc.EffectiveDate)
	}
	if len(doc.Sources) == 0 {
		return fmt.Errorf("sources required")
	}
	for i, s := range doc.Sources {
		u, err := url.Parse(strings.TrimSpace(s.URL))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || strings.TrimSpace(s.Claim) == "" {
			return fmt.Errorf("sources[%d] needs an http(s) url and a claim", i)
		}
	}
	if len(compact(doc.Facts)) == 0 {
		return fmt.Errorf("facts required")
	}
	d := doc.FormDelta
	if !formKey.MatchString(d.Stage) || strings.TrimSpace(d.Goal) == "" || len(compact(d.Acceptance)) == 0 {
		return fmt.Errorf("form_delta needs a stage matching %s, a goal, and acceptance lines", formKey)
	}
	for key := range d.Parameters {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("form_delta parameters must not have an empty key")
		}
	}
	return nil
}

// IntakePlatformSignal turns a platform_change signal into a proposed Form and
// a proposed work plan for every product whose channels or surfaces name the
// signal's surface, and one shared work plan for its machine prerequisites. It
// is idempotent: an unchanged form is left alone, and a work plan whose
// contract is already recorded is not recorded again. A changed form delta
// supersedes the form (same fact slots, keyed by the signal id) and re-plans
// the product work. dryRun computes the same plan and writes nothing.
func IntakePlatformSignal(homePath string, store *mem.Store, doc PlatformChangeDocument, sourcePath string, now time.Time, dryRun bool) (PlatformIntakeReport, error) {
	if err := ValidatePlatformChange(doc); err != nil {
		return PlatformIntakeReport{}, err
	}
	report := PlatformIntakeReport{
		SchemaVersion: PlatformIntakeSchemaVersion,
		Mode:          "recorded",
		SignalID:      doc.ID,
		Title:         doc.Title,
		Surface:       doc.Surface,
		EffectiveDate: doc.EffectiveDate,
		SourcePath:    sourcePath,
		Matched:       []PlatformIntakeProduct{},
		Skipped:       []PlatformIntakeSkip{},
	}
	if dryRun {
		report.Mode = "dry_run"
	}
	idx, err := loadWhereIndex(store)
	if err != nil {
		return report, err
	}

	prereqID := ""
	if len(compact(doc.Prerequisites)) > 0 {
		prereqID = stableID("work_", "product_platform_prerequisites", doc.ID)
	}
	for _, root := range idx.groups() {
		members := idx.members(root)
		entity, reason := idx.platformMatch(members, doc.Surface)
		if entity == "" {
			report.Skipped = append(report.Skipped, PlatformIntakeSkip{ProductID: members[0], Reason: reason})
			continue
		}
		entity = essenceEntity(members, entity, "", idx.claims)
		facts := idx.claims[entity]
		record := essenceRecordFromFacts(entity, facts)
		essenceBlock := "Product essence (" + entity + "): none recorded; declare product.essence in its product registry bundle and run `compound product import <product.yaml>`"
		if hasEssenceFacts(facts) {
			essenceBlock = "Product essence (" + entity + "): " + EssenceSummary(record.Essence)
		}
		checkout := productCheckout(facts)
		for _, m := range members {
			if checkout == "" {
				checkout = productCheckout(idx.claims[m])
			}
		}
		form := Form{Surface: doc.ID, Stage: doc.FormDelta.Stage, Goal: strings.TrimSpace(doc.FormDelta.Goal),
			Parameters: doc.FormDelta.Parameters, Acceptance: compact(doc.FormDelta.Acceptance), Status: "proposed"}
		recipe := productRecipe(checkout)
		report.Matched = append(report.Matched, PlatformIntakeProduct{
			ProductID:   entity,
			MatchReason: reason,
			Form:        form,
			FormStatus:  platformFormStatus(facts, form),
			Recipe:      recipe,
			Work:        platformProductWork(doc, entity, essenceBlock, checkout, recipe, prereqID),
		})
	}
	sort.Slice(report.Matched, func(i, j int) bool { return report.Matched[i].ProductID < report.Matched[j].ProductID })
	sort.Slice(report.Skipped, func(i, j int) bool { return report.Skipped[i].ProductID < report.Skipped[j].ProductID })
	if prereqID != "" && len(report.Matched) > 0 {
		item := platformPrerequisiteWork(doc, prereqID)
		report.Prerequisite = &item
	}

	var ws *work.Store
	if dryRun {
		ws, err = work.OpenReadOnly(homePath)
	} else {
		ws, err = work.Open(homePath)
	}
	if err != nil {
		return report, err
	}
	prior, err := platformPriorPlans(homePath)
	if err != nil {
		return report, err
	}
	if !dryRun && len(report.Matched) > 0 {
		evidence, err := store.RecordEvidence(mem.EvidenceItem{
			SourceKind: "platform_signal",
			SourceID:   "product-platform-signal:" + doc.ID,
			QuoteText:  fmt.Sprintf("%s (surface %s, effective %s): %s", doc.Title, doc.Surface, doc.EffectiveDate, strings.Join(compact(doc.Facts), " | ")),
			Actor:      "tool",
		}, now)
		if err != nil {
			return report, err
		}
		report.EvidenceID = evidence.ID
	}
	for i := range report.Matched {
		m := &report.Matched[i]
		if !dryRun && m.FormStatus != "unchanged" {
			if err := recordPlatformForm(store, m.ProductID, m.Form, report.EvidenceID, doc.ID, now); err != nil {
				return report, err
			}
		}
		if err := planPlatformWork(ws, &m.Work, prior[m.Work.WorkID], doc.ID, sourcePath, now, dryRun); err != nil {
			return report, err
		}
	}
	if report.Prerequisite != nil {
		if err := planPlatformWork(ws, report.Prerequisite, prior[report.Prerequisite.WorkID], doc.ID, sourcePath, now, dryRun); err != nil {
			return report, err
		}
	}
	report.Counts = platformCounts(report)
	return report, nil
}

// platformSurfaceAliases are the channel and surface-ref spellings that put a
// product on a signal surface besides the surface's own name. The ledger
// records channels and refs, not surface kinds, and this portfolio's App
// Store apps are iPhone apps, so the App Store channel and an App Store ref
// are what put a product on ios. No product field declares a platform (such
// as macOS-only) today, so nothing can exclude an App Store product from ios.
var platformSurfaceAliases = map[string]struct{ channels, refPrefixes []string }{
	"ios": {channels: []string{"app_store"}, refPrefixes: []string{"appstore:", "app_store:"}},
}

// platformMatch reports which member of a product carries the surface, and
// the exact channel or surface ref that matched — or, with an empty entity,
// why none does. A product is on surface s when a channel is s or a surface
// ref is s or s:<id>; failing that, when a channel or ref is one of s's
// aliases. An exact match outranks an alias, so the reason is the strongest.
func (idx *whereIndex) platformMatch(members []string, surface string) (string, string) {
	alias := platformSurfaceAliases[surface]
	channelOK := func(ch string, aliased bool) bool {
		if !aliased {
			return ch == surface
		}
		for _, a := range alias.channels {
			if ch == a {
				return true
			}
		}
		return false
	}
	refOK := func(ref string, aliased bool) bool {
		if !aliased {
			return ref == surface || strings.HasPrefix(ref, surface+":")
		}
		for _, prefix := range alias.refPrefixes {
			if strings.HasPrefix(ref, prefix) {
				return true
			}
		}
		return false
	}
	var channels, refs []string
	for _, aliased := range []bool{false, true} {
		for _, m := range members {
			if c, ok := idx.claim(m, "channels"); ok {
				for _, ch := range strings.Split(c.Object, ",") {
					ch = strings.TrimSpace(ch)
					channels = append(channels, ch)
					if channelOK(strings.ToLower(ch), aliased) {
						return m, "channel " + ch
					}
				}
			}
			if c, ok := idx.claim(m, "surfaces"); ok {
				for _, ref := range strings.Split(c.Object, ",") {
					ref = strings.TrimSpace(ref)
					refs = append(refs, ref)
					if refOK(strings.ToLower(ref), aliased) {
						return m, "surface " + ref
					}
				}
			}
		}
	}
	if len(compact(channels)) == 0 && len(compact(refs)) == 0 {
		return "", "no channels or surfaces recorded"
	}
	wantChannels := append([]string{surface}, alias.channels...)
	wantRefs := append([]string{surface, surface + ":<id>"}, alias.refPrefixes...)
	return "", fmt.Sprintf("channels [%s] include none of [%s] and none of %d surface refs is or starts with [%s]",
		strings.Join(compact(channels), ", "), strings.Join(wantChannels, ", "), len(compact(refs)), strings.Join(wantRefs, ", "))
}

// platformFormStatus compares the form the signal proposes with the form
// slots the product already holds. The status slot is not compared: a form a
// person moved past "proposed" stays where they put it until its delta changes.
func platformFormStatus(facts map[string]mem.Claim, form Form) string {
	present, same := false, true
	for _, f := range essenceFacts(Product{Forms: []Form{form}}) {
		claim, ok := facts[f.predicate]
		present = present || ok
		if !strings.HasSuffix(f.predicate, ".status") && (!ok || claim.Object != f.value) {
			same = false
		}
	}
	switch {
	case !present:
		return "new"
	case same:
		return "unchanged"
	}
	return "supersedes"
}

// recordPlatformForm writes the form.<signal id>.<stage>.* facts through the
// same capture and promote path the product registry uses; a changed value
// supersedes its own prior slot.
func recordPlatformForm(store *mem.Store, entity string, form Form, evidenceID, signalID string, now time.Time) error {
	src := "product-platform-signal:" + signalID
	var cands []mem.Candidate
	for _, f := range essenceFacts(Product{Forms: []Form{form}}) {
		cands = append(cands, mem.Candidate{
			Entity:         entity,
			Predicate:      f.predicate,
			Object:         f.value,
			Confidence:     1.0,
			Derivation:     "deterministic",
			ClaimType:      "status",
			Qualifiers:     map[string]string{"domain": "product"},
			PromotionBasis: candidates.BasisTrustedDeterministic,
			SourceTraceRef: evidenceID + ":" + src,
			SnippetHash:    stableID("prod_snip_", entity, f.predicate, f.value),
			Visibility:     "operational",
		})
	}
	captured, err := store.Capture(cands, now)
	if err != nil {
		return err
	}
	for _, c := range captured {
		if _, err := store.Adjudicate(c.ID, candidates.Promote, platformIntakeActor, now); err != nil {
			return err
		}
	}
	return nil
}

// productRecipe is the product checkout's verified recipe: the commands its
// compound-proof.json declares, accepted only if the canonical proof gate's
// own parser accepts the declaration.
func productRecipe(checkout string) string {
	if checkout == "" {
		return "no recipe recorded (no local checkout recorded for this product)"
	}
	path := filepath.Join(checkout, work.CanonicalConsumerContractPath)
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "no recipe recorded (no " + work.CanonicalConsumerContractPath + " in " + checkout + ")"
	}
	if err != nil {
		return "no recipe recorded (" + path + " unreadable: " + err.Error() + ")"
	}
	var decl struct {
		Consumers []struct {
			ID      string   `json:"id"`
			Command []string `json:"command"`
			Dir     string   `json:"dir"`
		} `json:"consumers"`
	}
	if err := json.Unmarshal(body, &decl); err != nil || len(decl.Consumers) == 0 {
		return "no recipe recorded (" + path + " declares no consumers)"
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	var runs []string
	for _, c := range decl.Consumers {
		step := work.ProofCheckStep{Name: "consumer:" + c.ID, Command: c.Command, Dir: c.Dir, ProofRef: "consumer:" + c.ID + ":sha256:" + digest}
		if _, err := work.CanonicalConsumerForStep(body, step); err != nil {
			return "no recipe recorded (" + path + " invalid: " + err.Error() + ")"
		}
		runs = append(runs, fmt.Sprintf("run `%s` in %s (consumer %s)", shellJoin(c.Command), filepath.Join(checkout, c.Dir), c.ID))
	}
	return "recipe " + path + " (sha256:" + digest[:12] + "): " + strings.Join(runs, "; ")
}

func platformSourceLines(doc PlatformChangeDocument) ([]string, []string) {
	var lines, urls []string
	for _, s := range doc.Sources {
		lines = append(lines, "signal source: "+strings.TrimSpace(s.URL)+" — "+strings.TrimSpace(s.Claim))
		urls = append(urls, strings.TrimSpace(s.URL))
	}
	return lines, urls
}

func platformProductWork(doc PlatformChangeDocument, entity, essenceBlock, checkout, recipe, prereqID string) PlatformWorkItem {
	d := doc.FormDelta
	formName := doc.ID + "/" + d.Stage
	goal := strings.TrimSpace(d.Goal)
	sourceLines, urls := platformSourceLines(doc)
	delta := "form delta " + formName + ": goal " + goal
	if len(d.Parameters) > 0 {
		delta += "; parameters " + formParameters(d.Parameters)
	}
	if checkout == "" {
		checkout = "none recorded"
	}
	inputs := []string{"product: " + entity, essenceBlock, delta, "checkout: " + checkout}
	nonGoals := []string{"do not submit, release, or change store metadata or prices", "do not change the product essence"}
	stops := []string{
		"an acceptance line cannot be checked on this machine",
		"meeting the form would break an essence constraint or anti-goal",
		"the change needs a store submission, release, price, or metadata change",
	}
	if prereqID != "" {
		inputs = append(inputs, "prerequisite work "+prereqID+" (filed once for this signal): "+strings.Join(compact(doc.Prerequisites), "; "))
		nonGoals = append(nonGoals, "do not install machine prerequisites here; work "+prereqID+" owns them")
		stops = append([]string{"a prerequisite owned by work " + prereqID + " is not satisfied on this machine"}, stops...)
	}
	inputs = append(inputs, sourceLines...)
	var checks []string
	for _, line := range compact(d.Acceptance) {
		checks = append(checks, "acceptance: "+line)
	}
	checks = append(checks, recipe)
	item := PlatformWorkItem{
		WorkID:           stableID("work_", "product_platform_signal", doc.ID, entity),
		Owner:            entity,
		State:            "proposed",
		SuccessPredicate: fmt.Sprintf("%s meets every acceptance line of form %s (%s), shown by its recipe or recorded acceptance evidence", entity, formName, goal),
		DesiredOutcome:   fmt.Sprintf("%s %s, per platform signal %s (%s; effective %s)", entity, goal, doc.ID, doc.Title, doc.EffectiveDate),
		NonGoals:         nonGoals,
		EvidenceRefs:     []string{platformSignalRef + doc.ID, "product_form:" + entity + ":" + doc.ID + "." + d.Stage},
		ActionContext: work.WorkActionContext{
			Kind:               platformWorkKind,
			SourceRef:          "compound://product-signals/platform/" + doc.ID,
			SourceID:           doc.ID,
			Title:              doc.Title + ": " + entity,
			ActionSummary:      fmt.Sprintf("Bring %s to proposed form %s (%s); signal effective %s. %s", entity, formName, goal, doc.EffectiveDate, essenceBlock),
			RequiredInputs:     inputs,
			ExpectedValidation: checks,
			StopConditions:     stops,
			Boundary:           "proposed work for " + entity + ": change only its checkout to meet form " + formName + "; no submission, release, store metadata, or price change",
			Proof:              recipe,
			EvidenceRefs:       append([]string{platformSignalRef + doc.ID}, urls...),
		},
	}
	return withPlatformContractRef(item)
}

func platformPrerequisiteWork(doc PlatformChangeDocument, workID string) PlatformWorkItem {
	prereqs := compact(doc.Prerequisites)
	sourceLines, urls := platformSourceLines(doc)
	var checks []string
	for _, p := range prereqs {
		checks = append(checks, "prerequisite: "+p)
	}
	checks = append(checks, "each prerequisite shown satisfied by a command's real output on this machine")
	item := PlatformWorkItem{
		WorkID:           workID,
		Owner:            platformIntakeActor,
		State:            "proposed",
		SuccessPredicate: fmt.Sprintf("every machine prerequisite of platform signal %s is satisfied: %s", doc.ID, strings.Join(prereqs, "; ")),
		DesiredOutcome:   fmt.Sprintf("machine prerequisites for platform signal %s (%s) installed once and proven, before its product work runs", doc.ID, doc.Title),
		NonGoals:         []string{"do not change any product checkout"},
		EvidenceRefs:     []string{platformSignalRef + doc.ID},
		ActionContext: work.WorkActionContext{
			Kind:               platformWorkKind,
			SourceRef:          "compound://product-signals/platform/" + doc.ID,
			SourceID:           doc.ID,
			Title:              doc.Title + ": machine prerequisites",
			ActionSummary:      fmt.Sprintf("Satisfy the machine prerequisites of platform signal %s once, for every product it reaches: %s", doc.ID, strings.Join(prereqs, "; ")),
			RequiredInputs:     append([]string{fmt.Sprintf("signal: %s (surface %s, effective %s)", doc.ID, doc.Surface, doc.EffectiveDate)}, sourceLines...),
			ExpectedValidation: checks,
			StopConditions:     []string{"an install needs the operator (GUI installer, account sign-in, or license prompt)"},
			Boundary:           "machine-level setup only; no product checkout changes",
			Proof:              "each prerequisite shown satisfied by a command's real output",
			EvidenceRefs:       append([]string{platformSignalRef + doc.ID}, urls...),
		},
	}
	return withPlatformContractRef(item)
}

// withPlatformContractRef binds the item to a digest of its contract, so a
// re-intake can tell an already-recorded plan from a changed one without
// comparing timestamps.
func withPlatformContractRef(item PlatformWorkItem) PlatformWorkItem {
	blob, _ := json.Marshal(struct {
		P, O string
		N    []string
		C    work.WorkActionContext
	}{item.SuccessPredicate, item.DesiredOutcome, item.NonGoals, item.ActionContext})
	item.ContractRef = platformContractRef + stableID("", string(blob))
	item.EvidenceRefs = append(item.EvidenceRefs, item.ContractRef)
	return item
}

// platformPriorPlans indexes recorded create-plans by work id.
func platformPriorPlans(homePath string) (map[string][]work.CreatePlan, error) {
	out := map[string][]work.CreatePlan{}
	dir := filepath.Join(homePath, "create-plans")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		plan, err := work.LoadCreatePlan(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // another writer's unreadable plan is not this intake's to judge
		}
		out[plan.WorkID] = append(out[plan.WorkID], plan)
	}
	return out, nil
}

// planPlatformWork records item as a proposed work create-plan through the
// work store's own writer (the one action-work-plan and release-work-plan
// use), unless the same contract is already recorded or the work was admitted.
func planPlatformWork(ws *work.Store, item *PlatformWorkItem, prior []work.CreatePlan, signalID, sourcePath string, now time.Time, dryRun bool) error {
	if existing, err := ws.Get(item.WorkID); err == nil {
		item.Status, item.State = "admitted", existing.State
		if ws.ValidateActionContextSnapshotForWork(existing, &item.ActionContext) != nil {
			item.Status = "admitted_changed"
			item.Note = "work already admitted; the signal's contract changed since — steer work " + item.WorkID + " instead of filing again"
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sort.Slice(prior, func(i, j int) bool { return prior[i].GeneratedAt < prior[j].GeneratedAt })
	for _, p := range prior {
		item.PriorPlanIDs = append(item.PriorPlanIDs, p.ID)
		for _, ref := range p.Work.EvidenceRefs {
			if ref == item.ContractRef {
				item.Status, item.PlanID, item.PlanPath, item.PriorPlanIDs = "existing", p.ID, p.PlanPath, nil
				item.Policy = fmt.Sprintf("%s allowed=%t", p.Policy.Outcome, p.Policy.Allowed)
				return nil
			}
		}
	}
	item.Status = "new"
	if len(item.PriorPlanIDs) > 0 {
		item.Status = "replans"
	}
	if dryRun {
		return nil
	}
	ctx := item.ActionContext
	plan, err := ws.CreatePlan(work.CreateInput{
		WorkID:           item.WorkID,
		Owner:            item.Owner,
		Actor:            platformIntakeActor,
		SuccessPredicate: item.SuccessPredicate,
		DesiredOutcome:   item.DesiredOutcome,
		NonGoals:         item.NonGoals,
		EvidenceRefs:     item.EvidenceRefs,
		ProposalKind:     envelope.ProposalKindOrdinary,
		State:            "proposed",
		Risk:             "tier2",
		Privacy:          "private",
	}, now, work.CreatePlanOptions{
		Actor:         platformIntakeActor,
		Capability:    "product.platform_signal",
		Action:        "plan_work_from_platform_signal",
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		TargetKind:    platformWorkKind,
		TargetID:      signalID + ":" + item.Owner,
		TargetPath:    sourcePath,
		ActionContext: &ctx,
		Record:        true,
	})
	if err != nil {
		return fmt.Errorf("plan work %s: %w", item.WorkID, err)
	}
	item.PlanID, item.PlanPath, item.State = plan.ID, plan.PlanPath, plan.WorkState
	item.Policy = fmt.Sprintf("%s allowed=%t", plan.Policy.Outcome, plan.Policy.Allowed)
	return nil
}

func platformCounts(r PlatformIntakeReport) PlatformIntakeCounts {
	c := PlatformIntakeCounts{Matched: len(r.Matched), Skipped: len(r.Skipped)}
	countWork := func(status string) {
		switch status {
		case "new":
			c.WorkNew++
		case "replans":
			c.WorkReplanned++
		default:
			c.WorkUnchanged++
		}
	}
	for _, m := range r.Matched {
		switch m.FormStatus {
		case "new":
			c.FormsNew++
		case "supersedes":
			c.FormsSuperseded++
		default:
			c.FormsUnchanged++
		}
		countWork(m.Work.Status)
	}
	if r.Prerequisite != nil {
		countWork(r.Prerequisite.Status)
	}
	return c
}

// RunPlatformSignalCLI is `compound product signal <platform_change.yaml>
// [--dry-run] [--json]`. handled is false when the arguments are not a
// platform_change document, so the metric-signal path keeps them.
func RunPlatformSignalCLI(args []string, homePath string, store *mem.Store, stdout, stderr io.Writer, now time.Time) (bool, int) {
	var path string
	var dryRun, asJSON bool
	for _, arg := range args {
		switch {
		case arg == "--dry-run":
			dryRun = true
		case arg == "--json":
			asJSON = true
		case strings.HasPrefix(arg, "-") || path != "":
			return false, 0
		default:
			path = arg
		}
	}
	if path == "" {
		return false, 0
	}
	doc, ok, err := LoadPlatformChange(path)
	if !ok {
		return false, 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "product signal:", err)
		return true, 1
	}
	report, err := IntakePlatformSignal(homePath, store, doc, path, now, dryRun)
	if err != nil {
		fmt.Fprintln(stderr, "product signal:", err)
		return true, 1
	}
	if dryRun || asJSON {
		blob, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintln(stderr, "product signal:", err)
			return true, 1
		}
		fmt.Fprintln(stdout, string(blob))
		return true, 0
	}
	fmt.Fprintf(stdout, "# product signal %s (%s)\n\n- status: %s\n- surface: %s\n- effective: %s\n", report.SignalID, PlatformChangeKind, report.Mode, report.Surface, report.EffectiveDate)
	if report.EvidenceID != "" {
		fmt.Fprintf(stdout, "- evidence: %s\n", report.EvidenceID)
	}
	for _, m := range report.Matched {
		fmt.Fprintf(stdout, "- product %s (%s): form %s/%s %s; work %s %s", m.ProductID, m.MatchReason, m.Form.Surface, m.Form.Stage, m.FormStatus, m.Work.WorkID, m.Work.Status)
		if m.Work.PlanPath != "" {
			fmt.Fprintf(stdout, " plan %s", m.Work.PlanPath)
		}
		fmt.Fprintln(stdout)
	}
	if p := report.Prerequisite; p != nil {
		fmt.Fprintf(stdout, "- prerequisite work %s %s", p.WorkID, p.Status)
		if p.PlanPath != "" {
			fmt.Fprintf(stdout, " plan %s", p.PlanPath)
		}
		fmt.Fprintln(stdout)
	}
	for _, s := range report.Skipped {
		fmt.Fprintf(stdout, "- skipped %s: %s\n", s.ProductID, s.Reason)
	}
	return true, 0
}

// TrySignalKinds handles the `product signal` inputs this package owns: a
// platform_change document, then a PostHog report export. It keeps
// cmd/compound a thin dispatcher; usage is productUsage() from the caller.
func TrySignalKinds(args []string, homePath string, store *mem.Store, usage string) bool {
	if handled, code := RunPlatformSignalCLI(args, homePath, store, os.Stdout, os.Stderr, time.Now()); handled {
		if code != 0 {
			os.Exit(code)
		}
		return true
	}
	return TryPostHogSignal(args, homePath, store, usage)
}
