package cli

// Compound `game` surface — studio-facing operator surface.
//
// Subcommands:
//   game doctor                                    — reachability + freshness summary (compound/game-doctor.v0.1)
//   game observe <steamweb|steamspy> --appids <csv> — pull signals into SourceObservation store
//   game genres                                    — genre catalog projection (compound/game-genres.v0.1)
//   game companies                                 — company catalog projection (compound/game-companies.v0.1)
//   game trending [--limit N] [--days D]            — top-N momentum view (compound/game-trending.v0.1)
//   game scout <query>                             — deterministic evidence packet (Read)
//   game draft <query>                             — LLM-rented one-pager; parks KindProductHypothesis (Governed)

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/distiller"
	"compound/internal/game"
	"compound/internal/game/idea"
	"compound/internal/game/release"
	"compound/internal/game/unity"
	"compound/internal/home"
	"compound/internal/mem"
)

const gameUsage = "usage: compound game <doctor|observe|genres|companies|trending|scout|draft|unity|release> [flags]\n" +
	"       compound game observe <steamweb|steamspy> --appids <csv> [--genres <csv>] [--companies <csv>] [--json] [--home P]\n" +
	"       compound game doctor [--json] [--home P]\n" +
	"       compound game genres [--json] [--home P]\n" +
	"       compound game companies [--json] [--home P]\n" +
	"       compound game trending [--limit N] [--days D] [--json] [--home P]\n" +
	"       compound game scout <query> [--limit N] [--json] [--home P]\n" +
	"       compound game draft <query> [--json] [--home P]\n" +
	"       compound game unity scaffold <idea-id> [--json] [--home P]\n" +
	"       compound game release plan --idea-id ID --product-id PID [--unity-artifact REF] [--change-summary S] [--rollback R] [--post-release-metric M] [--json] [--home P]\n" +
	"  Read/write game-source observations. scout reads; draft rents an LLM\n" +
	"  and parks a KindProductHypothesis candidate (never auto-promotes).\n" +
	"  unity scaffold emits a Unity project blueprint JSON; no disk writes in v0.\n" +
	"  release plan produces a ProductReleasePlan via product.BuildReleasePlan;\n" +
	"  it does NOT call Steamworks or upload builds."

func Run(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, gameUsage)
		os.Exit(2)
	}
	var code int
	switch args[0] {
	case "doctor":
		code = cmdGameDoctor(args[1:])
	case "observe":
		code = cmdGameObserve(args[1:])
	case "genres":
		code = cmdGameGenres(args[1:])
	case "companies":
		code = cmdGameCompanies(args[1:])
	case "trending":
		code = cmdGameTrending(args[1:])
	case "scout":
		code = cmdGameScout(args[1:])
	case "draft":
		code = cmdGameDraft(args[1:])
	case "unity":
		code = cmdGameUnity(args[1:])
	case "release":
		code = cmdGameRelease(args[1:])
	default:
		fmt.Fprintln(os.Stderr, gameUsage)
		os.Exit(2)
	}
	if code != 0 {
		os.Exit(code)
	}
}

func resolveGameHome(args []string) (string, error) {
	return home.Resolve(valueFlag(args, "--home"))
}

type gameObserveResult struct {
	Schema       string   `json:"schema"`
	AdapterID    string   `json:"adapter_id"`
	SourceSystem string   `json:"source_system"`
	Revision     string   `json:"revision,omitempty"`
	Written      int      `json:"written"`
	Failed       int      `json:"failed"`
	CandidateIDs []string `json:"candidate_ids,omitempty"`
	NoPromoteIDs []string `json:"no_promote_ids,omitempty"`
}

const gameObserveSchema = "compound/game-observe-result.v0.1"

func cmdGameObserve(args []string) int {
	if err := validateGameFlags("observe", args, map[string]bool{
		"--json": true, "--home": true, "--appids": true, "--genres": true, "--companies": true,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: adapter required (steamweb|steamspy)")
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
	adapter := args[0]
	appidsFlag := strings.TrimSpace(valueFlag(args, "--appids"))
	if appidsFlag == "" {
		fmt.Fprintln(os.Stderr, "error: --appids required")
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
	jsonOut := hasFlag(args, "--json")
	homePath, err := resolveGameHome(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	appids, err := parseGameAppIDs(appidsFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	genres := gameSplitCSV(valueFlag(args, "--genres"))
	companies := gameSplitCSV(valueFlag(args, "--companies"))

	result, err := runGameObserve(adapter, appids, genres, companies, homePath, time.Now().UTC())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	if jsonOut {
		printJSON(struct {
			Schema string            `json:"schema"`
			Result gameObserveResult `json:"result"`
		}{Schema: gameObserveSchema, Result: result})
		return 0
	}
	fmt.Printf("# game observe (%s)\n", gameObserveSchema)
	fmt.Printf("adapter:     %s\n", result.AdapterID)
	fmt.Printf("source_sys:  %s\n", result.SourceSystem)
	if result.Revision != "" {
		fmt.Printf("revision:    %s\n", result.Revision)
	}
	fmt.Printf("written:     %d\n", result.Written)
	if result.Failed > 0 {
		fmt.Printf("failed:      %d\n", result.Failed)
	}
	return 0
}

func cmdGameDoctor(args []string) int {
	if err := validateGameFlags("doctor", args, map[string]bool{"--json": true, "--home": true}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
	jsonOut := hasFlag(args, "--json")
	homePath, err := resolveGameHome(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	report, err := game.Doctor(homePath, time.Now().UTC())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if jsonOut {
		printJSON(report)
		return 0
	}
	fmt.Printf("# game doctor (%s)\n", report.Schema)
	fmt.Printf("generated: %s\n", report.Generated.Format(time.RFC3339))
	for _, w := range report.Warnings {
		fmt.Printf("warning:   %s\n", w)
	}
	if len(report.Counts) > 0 {
		fmt.Println("counts:")
		for _, k := range gameSortedStringKeys(report.Counts) {
			fmt.Printf("  %s: %d\n", k, report.Counts[k])
		}
	}
	if len(report.Findings) > 0 {
		fmt.Println("findings:")
		for _, f := range report.Findings {
			fmt.Printf("  %s — %s\n", f.Code, f.Detail)
		}
	} else {
		fmt.Println("findings: none")
	}
	return 0
}

func cmdGameGenres(args []string) int {
	if err := validateGameFlags("genres", args, map[string]bool{"--json": true, "--home": true}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
	jsonOut := hasFlag(args, "--json")
	homePath, err := resolveGameHome(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	report, err := game.ProjectGenres(homePath, time.Now().UTC())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if jsonOut {
		printJSON(report)
		return 0
	}
	printGameCatalogHuman(report, "genres")
	return 0
}

func cmdGameCompanies(args []string) int {
	if err := validateGameFlags("companies", args, map[string]bool{"--json": true, "--home": true}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
	jsonOut := hasFlag(args, "--json")
	homePath, err := resolveGameHome(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	report, err := game.ProjectCompanies(homePath, time.Now().UTC())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if jsonOut {
		printJSON(report)
		return 0
	}
	printGameCatalogHuman(report, "companies")
	return 0
}

func cmdGameTrending(args []string) int {
	if err := validateGameFlags("trending", args, map[string]bool{
		"--json": true, "--home": true, "--limit": true, "--days": true,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
	jsonOut := hasFlag(args, "--json")
	homePath, err := resolveGameHome(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	limit := parseGameIntFlag(valueFlag(args, "--limit"), 10)
	days := parseGameIntFlag(valueFlag(args, "--days"), 7)
	if limit <= 0 || limit > 1000 {
		fmt.Fprintln(os.Stderr, "error: --limit must be 1..1000")
		return 2
	}
	if days <= 0 || days > 365 {
		fmt.Fprintln(os.Stderr, "error: --days must be 1..365")
		return 2
	}
	report, err := game.ProjectTrending(homePath, time.Now().UTC(), limit, days)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if jsonOut {
		printJSON(report)
		return 0
	}
	fmt.Printf("# game trending (%s)\n", report.Schema)
	fmt.Printf("window:    %s\n", report.Window)
	if len(report.Entries) == 0 {
		fmt.Println("- no observations in window")
		return 0
	}
	for _, e := range report.Entries {
		fmt.Printf("- [%s] %s — %d hits (last %s)\n",
			e.Kind, e.Label, e.Hits, e.LastSeen.Format(time.RFC3339))
	}
	return 0
}

func printGameCatalogHuman(r game.CatalogReport, label string) {
	fmt.Printf("# game %s (%s)\n", label, r.Schema)
	fmt.Printf("generated: %s  stale_after: %s\n", r.Generated.Format(time.RFC3339), r.StaleAfter)
	for _, w := range r.Warnings {
		fmt.Printf("warning: %s\n", w)
	}
	if len(r.Entries) == 0 {
		fmt.Println("- (empty)")
		return
	}
	for _, e := range r.Entries {
		state := "fresh"
		if e.Stale {
			state = "stale"
		}
		apps := ""
		if len(e.Apps) > 0 {
			apps = fmt.Sprintf(" apps=%s", strings.Join(e.Apps, ","))
		}
		roles := ""
		if len(e.Roles) > 0 {
			roles = fmt.Sprintf(" roles=%s", strings.Join(e.Roles, ","))
		}
		fmt.Printf("- [%s] %s (%s, source=%s, n=%d)%s%s\n",
			state, e.Label, e.SourceKey, e.Source, e.ObservationCount, apps, roles)
	}
}

// cmdGameScout returns 0 on success, 2 on usage, 1 on op error.
func cmdGameScout(args []string) int {
	if err := validateGameFlags("scout", args, map[string]bool{"--json": true, "--home": true, "--limit": true}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
	jsonOut := hasFlag(args, "--json")
	homePath, err := resolveGameHome(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	query, err := gameFirstPositional(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
	limit := parseGameIntFlag(valueFlag(args, "--limit"), 20)

	pkt, err := idea.ComposeScoutPacket(context.Background(), homePath, query, time.Now().UTC(), limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: scout:", err)
		return 1
	}

	if jsonOut {
		printJSON(pkt)
		return 0
	}
	fmt.Printf("# game scout (%s)\n", pkt.Schema)
	fmt.Printf("query:      %s\n", pkt.Query)
	fmt.Printf("compiled:   %s\n", pkt.CompiledAt.Format(time.RFC3339))
	fmt.Printf("anchors:    %d\n", len(pkt.Anchors))
	for _, w := range pkt.Warnings {
		fmt.Printf("warning:    %s\n", w)
	}
	for _, a := range pkt.Anchors {
		apps := ""
		if len(a.AppIDs) > 0 {
			apps = fmt.Sprintf(" apps=%s", strings.Join(a.AppIDs, ","))
		}
		fmt.Printf("  - [%s] %s (key=%s, source=%s, n=%d)%s\n",
			a.Kind, a.Label, a.SourceKey, a.Source, a.Observation, apps)
	}
	if len(pkt.RecentMomentum) > 0 {
		fmt.Println("momentum:")
		for _, m := range pkt.RecentMomentum {
			fmt.Printf("  - [%s] %s — %d hits\n", m.Kind, m.Label, m.Hits)
		}
	}
	return 0
}

// cmdGameDraft returns 0 on success, 2 on usage, 1 on op error.
// The LLM call requires OPENAI_BASE_URL / OPENAI_API_KEY / OPENAI_MODEL
// env vars; the CLI surfaces a clear error if any are missing.
func cmdGameDraft(args []string) int {
	if err := validateGameFlags("draft", args, map[string]bool{"--json": true, "--home": true}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
	jsonOut := hasFlag(args, "--json")
	homePath, err := resolveGameHome(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	query, err := gameFirstPositional(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}

	baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	model := strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	if baseURL == "" || apiKey == "" || model == "" {
		fmt.Fprintln(os.Stderr, "error: OPENAI_BASE_URL, OPENAI_API_KEY, and OPENAI_MODEL must be set to rent a model lane")
		return 1
	}

	llm := &gameDraftLLMClient{
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
		BaseURL:    baseURL,
		APIKey:     apiKey,
		Model:      model,
	}
	res, err := idea.NewIdeaDraft(context.Background(), homePath, query, time.Now().UTC(), llm, idea.DefaultCandidateWriter{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	if jsonOut {
		printJSON(res)
		return 0
	}
	fmt.Printf("# game draft (%s)\n", res.Schema)
	fmt.Printf("query:        %s\n", res.Query)
	fmt.Printf("candidate:    %s\n", res.CandidateID)
	fmt.Printf("title:        %s\n", res.OnePager.Title)
	fmt.Printf("hook:         %s\n", res.OnePager.Hook)
	fmt.Printf("genres:       %s\n", strings.Join(res.OnePager.GenreIDs, ","))
	fmt.Printf("audience:     %s\n", res.OnePager.Audience)
	fmt.Printf("mechanic:     %s\n", res.OnePager.Mechanic)
	fmt.Printf("confidence:   %.2f\n", res.OnePager.Confidence)
	fmt.Printf("next_action:  compound product hypothesis-plan --candidate %s\n", res.CandidateID)
	return 0
}

// gameDraftLLMClient adapts the distiller.ChatCompletionJSON signature to
// idea.LLMClient's narrower contract.
type gameDraftLLMClient struct {
	HTTPClient *http.Client
	BaseURL    string
	APIKey     string
	Model      string
}

func (c *gameDraftLLMClient) Complete(ctx context.Context, system, user string) ([]byte, error) {
	content, _, _, err := distiller.ChatCompletionJSON(ctx, c.HTTPClient, c.BaseURL, c.APIKey, c.Model, system, user)
	return content, err
}

// gameFirstPositional extracts the first non-flag argument from args.
func gameFirstPositional(args []string) (string, error) {
	for _, a := range args {
		if strings.HasPrefix(a, "--") {
			continue
		}
		return strings.TrimSpace(a), nil
	}
	return "", fmt.Errorf("query argument required")
}

// cmdGameUnity returns 0 on success, 2 on usage, 1 on op error.
//
// Subcommands:
//
//	unity scaffold <idea-id>   — emit a Unity project blueprint JSON (Read; no disk writes in v0)
//
// The "scaffold" verb is fixed in v0 to keep the surface narrow. A future
// `--materialize` flag would land as a separate governed PR and require its
// own `withSubcommandFlagAdmission("unity", "--materialize", Governed)`
// line in command_registry.go.
func cmdGameUnity(args []string) int {
	if err := validateGameFlags("unity", args, map[string]bool{"--json": true, "--home": true}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: subcommand required (scaffold)")
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
	switch args[0] {
	case "scaffold":
		return cmdGameUnityScaffold(args[1:])
	default:
		fmt.Fprintln(os.Stderr, "error: unknown unity subcommand:", args[0])
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
}

// cmdGameUnityScaffold returns 0 on success, 2 on usage, 1 on op error.
// Pure spec emission — no filesystem writes, no Unity binary access.
func cmdGameUnityScaffold(args []string) int {
	if err := validateGameFlags("unity-scaffold", args, map[string]bool{"--json": true, "--home": true}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
	ideaID, err := gameFirstPositional(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
	if strings.TrimSpace(ideaID) == "" {
		fmt.Fprintln(os.Stderr, "error: idea-id required")
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
	// Refuse flag-shaped ids early — keeps the surface from accepting
	// accidental --materialize or similar future flags as a candidate id.
	if strings.HasPrefix(ideaID, "--") {
		fmt.Fprintln(os.Stderr, "error: idea-id cannot start with --")
		return 2
	}
	jsonOut := hasFlag(args, "--json")

	hints := unity.SourceHints{
		IdeaCandidate: ideaID,
	}
	bp, err := unity.Render(hints, time.Now().UTC())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	if jsonOut {
		// Ensure stable output even if Render set non-deterministic fields
		// by re-marshalling ourselves.
		out, mErr := json.MarshalIndent(bp, "", "  ")
		if mErr != nil {
			fmt.Fprintln(os.Stderr, "error: marshal:", mErr)
			return 1
		}
		_, _ = os.Stdout.Write(out)
		_, _ = os.Stdout.Write([]byte("\n"))
		return 0
	}
	fmt.Printf("# game unity scaffold (%s)\n", bp.Schema)
	fmt.Printf("idea:           %s\n", bp.Source.IdeaCandidate)
	fmt.Printf("unity_version:  %s\n", bp.UnityVersion)
	fmt.Printf("render_pipeline:%s\n", bp.RenderPipeline)
	fmt.Printf("project_root:   %s\n", bp.ProjectRoot)
	fmt.Println("boundary:")
	for _, b := range bp.Boundary {
		fmt.Printf("  - %s\n", b)
	}
	return 0
}

// candidates import is used transitively via idea.DefaultCandidateWriter.
// Keep the import here so go vet flags unused-import drift if the writer
// wiring ever moves into a separate package.
var _ = candidates.ProductHypothesis

// cmdGameRelease returns 0 on success, 2 on usage, 1 on op error.
//
// Subcommands:
//
//	release plan   — produce a ProductReleasePlan via product.BuildReleasePlan
//
// The release path is governance-only: it writes a plan JSON + review
// MD + review YAML under COMPOUND_HOME/product_release_plan/, but never
// calls Steamworks, never uploads a Unity build, never executes anything.
// Authorization is the next human step via `compound product release-review`.
func cmdGameRelease(args []string) int {
	if err := validateGameFlags("release", args, map[string]bool{
		"--json": true, "--home": true,
		"--idea-id": true, "--product-id": true, "--unity-artifact": true,
		"--change-summary": true, "--rollback": true, "--post-release-metric": true,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: subcommand required (plan)")
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
	switch args[0] {
	case "plan":
		return cmdGameReleasePlan(args[1:])
	default:
		fmt.Fprintln(os.Stderr, "error: unknown release subcommand:", args[0])
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}
}

func cmdGameReleasePlan(args []string) int {
	if err := validateGameFlags("release-plan", args, map[string]bool{
		"--json": true, "--home": true,
		"--idea-id": true, "--product-id": true, "--unity-artifact": true,
		"--change-summary": true, "--rollback": true, "--post-release-metric": true,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, gameUsage)
		return 2
	}

	ideaID := strings.TrimSpace(valueFlag(args, "--idea-id"))
	productID := strings.TrimSpace(valueFlag(args, "--product-id"))
	changeSummary := strings.TrimSpace(valueFlag(args, "--change-summary"))
	rollback := strings.TrimSpace(valueFlag(args, "--rollback"))
	postReleaseMetric := strings.TrimSpace(valueFlag(args, "--post-release-metric"))
	unityArtifact := strings.TrimSpace(valueFlag(args, "--unity-artifact"))

	for _, pair := range []struct {
		flag, val string
	}{
		{"--idea-id", ideaID},
		{"--product-id", productID},
		{"--change-summary", changeSummary},
		{"--rollback", rollback},
		{"--post-release-metric", postReleaseMetric},
	} {
		if pair.val == "" {
			fmt.Fprintln(os.Stderr, "error:", pair.flag, "required")
			fmt.Fprintln(os.Stderr, gameUsage)
			return 2
		}
	}

	jsonOut := hasFlag(args, "--json")
	homePath, err := resolveGameHome(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	res, err := release.Build(context.Background(), release.PlanInput{
		HomePath:          homePath,
		StoreOpener:       openGameMemStore,
		ProductID:         productID,
		IdeaCandidate:     ideaID,
		UnityArtifactRef:  unityArtifact,
		ChangeSummary:     changeSummary,
		Rollback:          rollback,
		PostReleaseMetric: postReleaseMetric,
	}, time.Now().UTC())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	if jsonOut {
		printJSON(res)
		return 0
	}
	fmt.Printf("# game release plan (%s)\n", res.Schema)
	fmt.Printf("plan_id:     %s\n", res.PlanID)
	fmt.Printf("plan_path:   %s\n", res.Paths.PlanPath)
	fmt.Printf("review_path: %s\n", res.Paths.ReviewPath)
	fmt.Printf("review_yaml: %s\n", res.Paths.ReviewYAMLPath)
	fmt.Printf("next_action: %s\n", res.NextAction)
	return 0
}

// openGameMemStore opens the compound memory store from homePath. Production
// wiring only — tests in the release package inject their own StoreOpener.
func openGameMemStore(homePath string) (*mem.Store, error) {
	// mem.Open (canonical) lives in internal/mem. We import the package
	// here so cmd/compound stays the only place that knows the concrete
	// constructor. Open returns nil store + error if the home is empty
	// or unreadable; Build translates that to a clear CLI error.
	return mem.Open(homePath)
}

func validateGameFlags(subcommand string, args []string, allowed map[string]bool) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			continue
		}
		name := a
		if eq := strings.Index(a, "="); eq >= 0 {
			name = a[:eq]
		}
		if i == 0 && subcommand == "observe" {
			continue
		}
		if !allowed[name] {
			return fmt.Errorf("%s: unknown flag %s", subcommand, a)
		}
		switch name {
		case "--home", "--appids", "--genres", "--companies", "--limit", "--days":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++
			}
		}
	}
	return nil
}

func parseGameAppIDs(csv string) ([]int64, error) {
	parts := gameSplitCSV(csv)
	if len(parts) == 0 {
		return nil, fmt.Errorf("--appids must contain at least one appid")
	}
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		var v int64
		if _, err := fmt.Sscanf(p, "%d", &v); err != nil {
			return nil, fmt.Errorf("invalid appid %q: %w", p, err)
		}
		if v <= 0 {
			return nil, fmt.Errorf("invalid appid %q (must be positive)", p)
		}
		out = append(out, v)
	}
	return out, nil
}

func gameSplitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseGameIntFlag(s string, fallback int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	var v int
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return fallback
	}
	return v
}

func gameSortedStringKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
