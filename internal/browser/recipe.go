package browser

// recipe.go — browser self-heal, part 2: recorded recipes and the run engine.
//
// A recipe is an ordered list of steps; element steps carry a semantic
// descriptor (role + name/text/placeholder) and the agent-browser ref that
// last matched it (cached_ref). The engine follows the cache-then-repair
// pattern: each element step takes one accessibility snapshot, uses the
// cached ref when that ref still names an element matching the descriptor,
// and otherwise (or when acting on it fails) re-finds the element from the
// snapshot by deterministic ranking and records the new ref. A cached ref
// that now names a DIFFERENT element is never acted on. No match fails the
// step and stops the run; every run leaves a receipt with per-step outcome
// cached|healed|failed and timing, and never a typed value.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	recipeSchema        = "compound.browser_recipe.v0.1"
	runReceiptSchema    = "compound.browser_run_receipt.v0.1"
	maxRecipeSteps      = 100
	defaultWaitTimeout  = 10 * time.Second
	waitPollInterval    = 500 * time.Millisecond
	outcomeCached       = "cached"
	outcomeHealed       = "healed"
	outcomeFailed       = "failed"
	valueSourceLiteral  = "literal"
	valueSourceParam    = "param"
	valueSourceSecret   = "secret"
	runPurposeRun       = "run"
	runPurposeHealLogin = "heal_login"
)

var (
	recipeNamePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	templateToken      = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.:-]+)\s*\}\}`)
	recipeActionValues = map[string]bool{"navigate": true, "click": true, "fill": true, "press": true, "wait": true, "assert": true}
)

type recipeDescriptor struct {
	Role        string `json:"role,omitempty"`
	Text        string `json:"text,omitempty"`
	Name        string `json:"name,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	URL         string `json:"url,omitempty"`
}

func (d recipeDescriptor) hasElementKey() bool {
	return strings.TrimSpace(d.Name) != "" || strings.TrimSpace(d.Text) != "" || strings.TrimSpace(d.Placeholder) != ""
}

type recipeStep struct {
	Action     string           `json:"action"`
	Descriptor recipeDescriptor `json:"descriptor,omitzero"`
	CachedRef  string           `json:"cached_ref,omitempty"`
	Value      string           `json:"value,omitempty"`
}

type browserRecipe struct {
	SchemaVersion string       `json:"schema_version"`
	Name          string       `json:"name"`
	Profile       string       `json:"profile"`
	Description   string       `json:"description,omitempty"`
	Revision      int          `json:"revision"`
	UpdatedAt     string       `json:"updated_at,omitempty"`
	Steps         []recipeStep `json:"steps"`
}

type recipeSummary struct {
	Profile   string `json:"profile"`
	Name      string `json:"name"`
	Steps     int    `json:"steps"`
	Revision  int    `json:"revision"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

func validRecipeName(name string) error {
	if !recipeNamePattern.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("invalid recipe name %q", name)
	}
	return nil
}

func recipePath(home, profile, name string) string {
	return selfHealDir(home, "recipes", profile, name+".json")
}

func loadRecipe(home, profile, name string) (browserRecipe, error) {
	if err := validSelfHealID("profile", profile); err != nil {
		return browserRecipe{}, err
	}
	if err := validRecipeName(name); err != nil {
		return browserRecipe{}, err
	}
	body, err := os.ReadFile(recipePath(home, profile, name))
	if os.IsNotExist(err) {
		return browserRecipe{}, fmt.Errorf("recipe_missing: no recipe %s/%s", profile, name)
	}
	if err != nil {
		return browserRecipe{}, err
	}
	var r browserRecipe
	if err := json.Unmarshal(body, &r); err != nil {
		return browserRecipe{}, fmt.Errorf("recipe_invalid: %v", err)
	}
	if err := validateRecipe(r, profile, name); err != nil {
		return browserRecipe{}, err
	}
	return r, nil
}

func validateRecipe(r browserRecipe, profile, name string) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("recipe_invalid: "+format, args...)
	}
	if r.SchemaVersion != recipeSchema {
		return bad("schema_version %q is not %q", r.SchemaVersion, recipeSchema)
	}
	if r.Name != name || r.Profile != profile {
		return bad("name/profile %q/%q do not match the file %s/%s", r.Name, r.Profile, profile, name)
	}
	if len(r.Steps) == 0 || len(r.Steps) > maxRecipeSteps {
		return bad("needs 1..%d steps", maxRecipeSteps)
	}
	for i, s := range r.Steps {
		if !recipeActionValues[s.Action] {
			return bad("step %d: unknown action %q", i, s.Action)
		}
		if s.CachedRef != "" && refOrdinal(s.CachedRef) == 1<<30 {
			return bad("step %d: cached_ref %q is not an agent-browser ref", i, s.CachedRef)
		}
		if strings.Contains(s.Descriptor.URL, "{{secret:") || (s.Action != "fill" && strings.Contains(s.Value, "{{secret:")) {
			return bad("step %d: secrets are only allowed as a fill value", i)
		}
		if strings.Contains(s.Value, "{{secret:") && !isWholeSecret(s.Value) {
			return bad("step %d: a secret fill value must be exactly {{secret:FIELD}}", i)
		}
		switch s.Action {
		case "navigate":
			if s.Descriptor.URL == "" {
				return bad("step %d: navigate needs descriptor.url", i)
			}
		case "click", "fill":
			if !s.Descriptor.hasElementKey() {
				return bad("step %d: %s needs descriptor name, text or placeholder", i, s.Action)
			}
			if s.Action == "fill" && s.Value == "" {
				return bad("step %d: fill needs a value", i)
			}
		case "press":
			if s.Value == "" {
				return bad("step %d: press needs a key value", i)
			}
		case "wait":
			if s.Value == "" && !s.Descriptor.hasElementKey() && s.Descriptor.Role == "" {
				return bad("step %d: wait needs milliseconds or a descriptor", i)
			}
		case "assert":
			if !s.Descriptor.hasElementKey() && s.Descriptor.Role == "" && s.Descriptor.URL == "" {
				return bad("step %d: assert needs a descriptor", i)
			}
		}
	}
	return nil
}

func isWholeSecret(value string) bool {
	m := templateToken.FindStringSubmatch(strings.TrimSpace(value))
	return m != nil && m[0] == strings.TrimSpace(value) && strings.HasPrefix(m[1], "secret:")
}

func secretField(value string) string {
	if !isWholeSecret(value) {
		return ""
	}
	return strings.TrimPrefix(templateToken.FindStringSubmatch(strings.TrimSpace(value))[1], "secret:")
}

// recipeRequirements lists the params and secret fields a recipe needs.
func recipeRequirements(r browserRecipe) (params, secrets []string) {
	seenP, seenS := map[string]bool{}, map[string]bool{}
	for _, s := range r.Steps {
		for _, text := range []string{s.Descriptor.URL, s.Value} {
			for _, m := range templateToken.FindAllStringSubmatch(text, -1) {
				if field, ok := strings.CutPrefix(m[1], "secret:"); ok {
					if !seenS[field] {
						seenS[field] = true
						secrets = append(secrets, field)
					}
					continue
				}
				if !seenP[m[1]] {
					seenP[m[1]] = true
					params = append(params, m[1])
				}
			}
		}
	}
	return params, secrets
}

func substituteParams(text string, params map[string]string) string {
	return templateToken.ReplaceAllStringFunc(text, func(tok string) string {
		name := templateToken.FindStringSubmatch(tok)[1]
		if v, ok := params[name]; ok {
			return v
		}
		return tok
	})
}

func listRecipes(home, profile string) ([]recipeSummary, error) {
	pattern := selfHealDir(home, "recipes", "*", "*.json")
	if profile != "" {
		if err := validSelfHealID("profile", profile); err != nil {
			return nil, err
		}
		pattern = selfHealDir(home, "recipes", profile, "*.json")
	}
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	out := []recipeSummary{}
	for _, path := range paths {
		prof := filepath.Base(filepath.Dir(path))
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		r, err := loadRecipe(home, prof, name)
		if err != nil {
			out = append(out, recipeSummary{Profile: prof, Name: name})
			continue
		}
		out = append(out, recipeSummary{Profile: prof, Name: name, Steps: len(r.Steps), Revision: r.Revision, UpdatedAt: r.UpdatedAt})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Profile != out[j].Profile {
			return out[i].Profile < out[j].Profile
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func saveRecipe(home string, r browserRecipe) error {
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(recipePath(home, r.Profile, r.Name), append(body, '\n'), 0o644)
}

// --- ranking ---------------------------------------------------------------

// descriptorScore is 0 when the node does not match every set descriptor
// field; otherwise exact matches outrank substring matches.
func descriptorScore(n axNode, d recipeDescriptor) int {
	if role := normalizeText(d.Role); role != "" && normalizeText(n.Role) != role {
		return 0
	}
	name := normalizeText(n.Name)
	score := 0
	for _, f := range []struct {
		want          string
		exact, substr int
	}{{d.Name, 100, 40}, {d.Text, 90, 50}, {d.Placeholder, 60, 30}} {
		want := normalizeText(f.want)
		switch {
		case want == "":
		case name == want:
			score += f.exact
		case strings.Contains(name, want):
			score += f.substr
		default:
			return 0
		}
	}
	if d.Role != "" {
		score += 10 // a role-only descriptor matches by role alone
	}
	return score
}

// rankCandidates orders matching nodes by score, then document order, so
// the same snapshot always yields the same choice.
func rankCandidates(nodes []axNode, d recipeDescriptor) []axNode {
	type scored struct {
		node  axNode
		score int
	}
	var hits []scored
	for _, n := range nodes {
		if s := descriptorScore(n, d); s > 0 {
			hits = append(hits, scored{n, s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return refOrdinal(hits[i].node.Ref) < refOrdinal(hits[j].node.Ref)
	})
	out := make([]axNode, len(hits))
	for i, h := range hits {
		out[i] = h.node
	}
	return out
}

// --- run engine ------------------------------------------------------------

type stepReceipt struct {
	Index       int    `json:"index"`
	Action      string `json:"action"`
	Outcome     string `json:"outcome"`
	RefBefore   string `json:"ref_before,omitempty"`
	RefAfter    string `json:"ref_after,omitempty"`
	Candidates  int    `json:"candidates,omitempty"`
	ValueSource string `json:"value_source,omitempty"`
	DurationMS  int64  `json:"duration_ms"`
	Error       string `json:"error,omitempty"`
}

type runReceipt struct {
	Schema         string        `json:"schema"`
	ID             string        `json:"id"`
	Profile        string        `json:"profile"`
	Recipe         string        `json:"recipe"`
	RecipeRevision int           `json:"recipe_revision"`
	Purpose        string        `json:"purpose"`
	StartedAt      string        `json:"started_at"`
	FinishedAt     string        `json:"finished_at"`
	DurationMS     int64         `json:"duration_ms"`
	Status         string        `json:"status"`
	HealedSteps    int           `json:"healed_steps"`
	RecipeUpdated  bool          `json:"recipe_updated"`
	Steps          []stepReceipt `json:"steps"`
}

// secretResolver returns one credential field. Values stay in memory, travel
// to agent-browser on stdin only, and are redacted from receipts.
type secretResolver func(ctx context.Context, field string) (string, error)

type recipeExec struct {
	client  *agentBrowserClient
	params  map[string]string
	secrets secretResolver
	redact  []string
}

func newRunID(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "brun_" + now.Format("20060102T150405.000000000Z") + "_" + hex.EncodeToString(b[:])
}

// executeRecipe runs a validated recipe, persists healed refs back into the
// recipe, and writes the run receipt. The returned error is infrastructure
// only; a failed step is reported through receipt.Status.
func executeRecipe(ctx context.Context, home string, client *agentBrowserClient, r browserRecipe, params map[string]string, secrets secretResolver, purpose string) (runReceipt, error) {
	started := selfHealNow()
	receipt := runReceipt{Schema: runReceiptSchema, ID: newRunID(started), Profile: r.Profile, Recipe: r.Name,
		RecipeRevision: r.Revision, Purpose: purpose, StartedAt: started.Format(time.RFC3339Nano), Status: "ok", Steps: []stepReceipt{}}
	ex := &recipeExec{client: client, params: params, secrets: secrets}
	for _, v := range params {
		if len(strings.TrimSpace(v)) >= 4 { // shorter params would mangle every error string
			ex.redact = append(ex.redact, v)
		}
	}
	changed := false
	for i := range r.Steps {
		step := &r.Steps[i]
		before := step.CachedRef
		t0 := time.Now()
		sr := ex.runStep(ctx, i, step)
		sr.DurationMS = time.Since(t0).Milliseconds()
		sr.Error = ex.scrub(sr.Error)
		receipt.Steps = append(receipt.Steps, sr)
		if step.CachedRef != before {
			changed = true
		}
		if sr.Outcome == outcomeHealed {
			receipt.HealedSteps++
		}
		if sr.Outcome == outcomeFailed {
			receipt.Status = "failed"
			break
		}
	}
	finished := selfHealNow()
	receipt.FinishedAt = finished.Format(time.RFC3339Nano)
	receipt.DurationMS = finished.Sub(started).Milliseconds()
	if changed {
		r.Revision++
		r.UpdatedAt = finished.Format(time.RFC3339Nano)
		if err := saveRecipe(home, r); err != nil {
			return receipt, fmt.Errorf("save healed recipe: %w", err)
		}
		receipt.RecipeUpdated = true
	}
	body, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return receipt, err
	}
	if err := writeFileAtomic(selfHealDir(home, "runs", receipt.ID+".json"), append(body, '\n'), 0o644); err != nil {
		return receipt, fmt.Errorf("write run receipt: %w", err)
	}
	return receipt, nil
}

func (ex *recipeExec) scrub(s string) string {
	for _, v := range ex.redact {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	return s
}

func (ex *recipeExec) runStep(ctx context.Context, i int, step *recipeStep) stepReceipt {
	sr := stepReceipt{Index: i, Action: step.Action, Outcome: outcomeCached}
	fail := func(err error) stepReceipt {
		sr.Outcome = outcomeFailed
		sr.Error = truncateDetail(err.Error())
		return sr
	}
	switch step.Action {
	case "navigate":
		if err := ex.client.open(ctx, substituteParams(step.Descriptor.URL, ex.params)); err != nil {
			return fail(err)
		}
		return sr
	case "press":
		if _, err := ex.client.call(ctx, nil, "press", substituteParams(step.Value, ex.params)); err != nil {
			return fail(err)
		}
		return sr
	case "wait":
		return ex.waitStep(ctx, step, sr)
	case "assert":
		ok, err := ex.assertStep(ctx, step.Descriptor)
		if err != nil {
			return fail(err)
		}
		if !ok {
			return fail(errors.New("assert_failed: page does not match the descriptor"))
		}
		return sr
	case "click":
		return ex.elementStep(ctx, step, sr, func(ref string) error {
			_, err := ex.client.call(ctx, nil, "click", "@"+ref)
			return err
		})
	case "fill":
		value, source, err := ex.fillValue(ctx, step.Value)
		sr.ValueSource = source
		if err != nil {
			return fail(err)
		}
		return ex.elementStep(ctx, step, sr, func(ref string) error { return ex.fillViaStdin(ctx, ref, value) })
	}
	return fail(fmt.Errorf("unknown action %q", step.Action))
}

func (ex *recipeExec) fillValue(ctx context.Context, raw string) (string, string, error) {
	if field := secretField(raw); field != "" {
		if ex.secrets == nil {
			return "", valueSourceSecret, errors.New("credential_provider_missing: recipe needs a secret and no provider is available")
		}
		value, err := ex.secrets(ctx, field)
		if err != nil {
			return "", valueSourceSecret, fmt.Errorf("credential_unavailable: field %s", field)
		}
		ex.redact = append(ex.redact, value)
		return value, valueSourceSecret, nil
	}
	if templateToken.MatchString(raw) {
		return substituteParams(raw, ex.params), valueSourceParam, nil
	}
	return raw, valueSourceLiteral, nil
}

// fillViaStdin sends the fill through `agent-browser batch` on stdin so the
// value never appears in a process argument list.
func (ex *recipeExec) fillViaStdin(ctx context.Context, ref, value string) error {
	stdin, err := json.Marshal([][]string{{"fill", "@" + ref, value}})
	if err != nil {
		return err
	}
	raw, execErr := ex.client.exec(ctx, stdin, "batch", "--bail")
	var results []struct {
		Success bool            `json:"success"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &results); err != nil || len(results) != 1 {
		if execErr != nil {
			return execErr
		}
		return errors.New("agent-browser batch: undecodable output")
	}
	if !results[0].Success {
		var msg string
		_ = json.Unmarshal(results[0].Error, &msg)
		return fmt.Errorf("agent-browser fill: %s", truncateDetail(msg))
	}
	return execErr
}

// elementStep resolves the step's element from a fresh snapshot (cached ref
// when it still matches, else re-found), acts on it, and on a stale cached
// ref re-snapshots and re-finds once.
func (ex *recipeExec) elementStep(ctx context.Context, step *recipeStep, sr stepReceipt, act func(string) error) stepReceipt {
	sr.RefBefore = step.CachedRef
	fail := func(err error) stepReceipt {
		sr.Outcome = outcomeFailed
		sr.Error = truncateDetail(err.Error())
		return sr
	}
	nodes, err := ex.client.snapshot(ctx)
	if err != nil {
		return fail(err)
	}
	candidates := rankCandidates(nodes, step.Descriptor)
	sr.Candidates = len(candidates)
	ref, healed := "", false
	if step.CachedRef != "" {
		for _, n := range nodes {
			if n.Ref == step.CachedRef && descriptorScore(n, step.Descriptor) > 0 {
				ref = n.Ref
			}
		}
	}
	if ref == "" {
		if len(candidates) == 0 {
			return fail(errors.New("descriptor_not_found: no element in the accessibility snapshot matches the descriptor"))
		}
		ref, healed = candidates[0].Ref, true
	}
	if err := act(ref); err != nil {
		if healed {
			return fail(err)
		}
		nodes, err = ex.client.snapshot(ctx)
		if err != nil {
			return fail(err)
		}
		candidates = rankCandidates(nodes, step.Descriptor)
		sr.Candidates = len(candidates)
		if len(candidates) == 0 {
			return fail(errors.New("descriptor_not_found: cached ref went stale and no element matches the descriptor"))
		}
		ref, healed = candidates[0].Ref, true
		if err := act(ref); err != nil {
			return fail(err)
		}
	}
	if healed {
		sr.Outcome = outcomeHealed
	}
	step.CachedRef = ref
	sr.RefAfter = ref
	return sr
}

func (ex *recipeExec) waitStep(ctx context.Context, step *recipeStep, sr stepReceipt) stepReceipt {
	value := substituteParams(step.Value, ex.params)
	if !step.Descriptor.hasElementKey() && step.Descriptor.Role == "" {
		if _, err := strconv.Atoi(value); err != nil {
			sr.Outcome, sr.Error = outcomeFailed, "wait value must be milliseconds"
			return sr
		}
		if _, err := ex.client.call(ctx, nil, "wait", value); err != nil {
			sr.Outcome, sr.Error = outcomeFailed, truncateDetail(err.Error())
		}
		return sr
	}
	timeout := defaultWaitTimeout
	if ms, err := strconv.Atoi(value); err == nil && ms > 0 {
		timeout = time.Duration(ms) * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for {
		nodes, err := ex.client.snapshot(ctx)
		if err == nil && len(rankCandidates(nodes, step.Descriptor)) > 0 {
			return sr
		}
		if time.Now().After(deadline) {
			sr.Outcome, sr.Error = outcomeFailed, "descriptor_not_found: wait timed out"
			return sr
		}
		select {
		case <-ctx.Done():
			sr.Outcome, sr.Error = outcomeFailed, ctx.Err().Error()
			return sr
		case <-time.After(waitPollInterval):
		}
	}
}

func (ex *recipeExec) assertStep(ctx context.Context, d recipeDescriptor) (bool, error) {
	if pattern := strings.TrimSpace(d.URL); pattern != "" {
		re, err := regexp.Compile(substituteParams(pattern, ex.params))
		if err != nil {
			return false, fmt.Errorf("assert url pattern: %w", err)
		}
		url, err := ex.client.currentURL(ctx)
		if err != nil {
			return false, err
		}
		if !re.MatchString(url) {
			return false, nil
		}
	}
	elementKey := d.Role != "" || d.Name != "" || d.Placeholder != ""
	if d.Text != "" && !elementKey {
		page, err := ex.client.pageText(ctx)
		if err != nil {
			return false, err
		}
		return strings.Contains(normalizeText(page), normalizeText(d.Text)), nil
	}
	if elementKey {
		nodes, err := ex.client.snapshot(ctx)
		if err != nil {
			return false, err
		}
		return len(rankCandidates(nodes, d)) > 0, nil
	}
	return true, nil
}
