package product

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/mem"
)

// registryBundle is the shape the product-registry writer imports: surfaces and
// channels, no deployment. Its ids keep hyphens (product:tide-log), while the
// contract importer's normalizer produces underscores (product:tide_log) —
// the split that left one product as several ledger entities.
func registryBundle(id, name string, aliases ...string) Bundle {
	return Bundle{
		SchemaVersion: SchemaVersion,
		Product: Product{
			ID:             id,
			Name:           name,
			Aliases:        aliases,
			Surfaces:       []Surface{{ID: "app-store", Kind: "app_store", Ref: "appstore:" + strings.TrimPrefix(id, "product:")}},
			Channels:       []string{"app_store"},
			Positioning:    name + " fixture",
			PrivacyPosture: "unknown",
		},
		EvidenceRefs: []string{"fixture:" + id},
	}
}

func deployedBundle(id, name, place, command string, aliases ...string) Bundle {
	b := registryBundle(id, name, aliases...)
	b.Product.Deployment = Deployment{Place: place, Method: "launchd_in_place_repo", Command: command}
	return b
}

func whereStore(t *testing.T, bundles ...Bundle) *mem.Store {
	t.Helper()
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bundles {
		if _, err := Apply(home, store, b, b.Product.ID+".yaml", tProduct); err != nil {
			t.Fatalf("apply %s: %v", b.Product.ID, err)
		}
	}
	return store
}

// TestWhereJoinsSpellingVariantsOfOneProduct is the depth test for the reader:
// the deployment lives on the contract entity, the query names the registry
// spelling, and the answer must still be the contract's place and command. A
// reader that only looks up the exact entity id answers "unknown" here — which is
// what every agent got before, while the fact sat one underscore away.
func TestWhereJoinsSpellingVariantsOfOneProduct(t *testing.T) {
	store := whereStore(t,
		registryBundle("product:tide-log", "Tide Log"),
		registryBundle("product:tidelog", "Tide Log"),
		deployedBundle("product:tide_log", "Tide Log", "place:build-host", "make deploy"),
	)
	for _, query := range []string{"tidelog", "tide-log", "product:tide_log", "Tide Log"} {
		got, err := Where(store, query)
		if err != nil {
			t.Fatalf("where %q: %v", query, err)
		}
		if got.Status != WhereKnown {
			t.Fatalf("where %q status = %q, want %q (%+v)", query, got.Status, WhereKnown, got)
		}
		if got.Deployment.Place != "place:build-host" || got.Deployment.Command != "make deploy" {
			t.Errorf("where %q deployment = %+v, want the contract's place and command", query, got.Deployment)
		}
		if got.ProductID != "product:tide_log" {
			t.Errorf("where %q product = %q, want the member that carries the deployment", query, got.ProductID)
		}
		if want := []string{"product:tide-log", "product:tide_log", "product:tidelog"}; strings.Join(got.Members, ",") != strings.Join(want, ",") {
			t.Errorf("where %q members = %v, want %v", query, got.Members, want)
		}
	}
}

// TestWhereFollowsContractAliases covers a rename the spelling fold cannot see:
// the contract's slug is `inkwell` and it lists Quillmark as an alias, while
// the registry still holds product:quillmark. Only the declared alias joins them.
func TestWhereFollowsContractAliases(t *testing.T) {
	store := whereStore(t,
		registryBundle("product:quillmark", "Quillmark"),
		deployedBundle("product:inkwell", "Inkwell", "place:cloudflare-pages", "npm run deploy", "Quillmark"),
	)
	got, err := Where(store, "quillmark")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != WhereKnown || got.Deployment.Place != "place:cloudflare-pages" {
		t.Fatalf("where quillmark = %+v, want inkwell's deployment via its declared alias", got)
	}
}

// TestWhereUnknownNeverInventsACommand pins the answer for a product with no
// contract: status unknown, an empty deployment, and a next step that sends the
// agent to ask. An agent will run whatever command this reader prints.
func TestWhereUnknownNeverInventsACommand(t *testing.T) {
	store := whereStore(t, registryBundle("product:sundial", "Sundial"))
	got, err := Where(store, "sundial")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != WhereUnknown {
		t.Fatalf("status = %q, want %q", got.Status, WhereUnknown)
	}
	if got.Deployment.Place != "" || got.Deployment.Command != "" {
		t.Errorf("deployment = %+v, want empty — nothing in the ledger says where it runs", got.Deployment)
	}
	if !strings.Contains(got.Next, "do not infer") {
		t.Errorf("next = %q, want an instruction not to infer a deploy command", got.Next)
	}
	if len(got.Surfaces) == 0 {
		t.Errorf("surfaces empty; where it is sold is still worth reporting when where it runs is unknown")
	}
}

// TestWhereReportsConflictWithoutPicking: two records of the same target claim
// different places. Choosing one would aim a deploy at a machine the other
// record says is wrong, so the reader names both and resolves neither.
func TestWhereReportsConflictWithoutPicking(t *testing.T) {
	// Both records come from one checkout — a slug renamed in place — so they
	// describe the same target and cannot both be right.
	renamedFrom := deployedBundle("product:ledger-99", "Ledger 99", "place:build-host", "make deploy")
	renamedTo := deployedBundle("product:ledger_99", "Ledger 99", "place:apps.example", "docker compose up -d")
	for _, b := range []*Bundle{&renamedFrom, &renamedTo} {
		b.Product.Surfaces = append(b.Product.Surfaces, Surface{ID: "checkout", Kind: "repo", Ref: "/workspace/products/ledger"})
	}
	store := whereStore(t, renamedFrom, renamedTo)
	got, err := Where(store, "ledger99")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != WhereConflict {
		t.Fatalf("status = %q, want %q", got.Status, WhereConflict)
	}
	if got.Deployment.Place != "" || got.Deployment.Command != "" {
		t.Errorf("deployment = %+v, want empty on conflict", got.Deployment)
	}
	if strings.Join(got.Conflicts, ",") != "place:apps.example,place:build-host" {
		t.Errorf("conflicts = %v, want both places", got.Conflicts)
	}
}

func TestWhereUnresolvedQuery(t *testing.T) {
	store := whereStore(t, registryBundle("product:sundial", "Sundial"))
	got, err := Where(store, "no-such-product")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != WhereUnresolved || got.ProductID != "" {
		t.Fatalf("got %+v, want unresolved with no product", got)
	}
}

// TestWhereResolvesByRepoPath lets an agent ask from inside a checkout: a path
// under a product's recorded source path resolves to that product.
func TestWhereResolvesByRepoPath(t *testing.T) {
	b := deployedBundle("product:cask", "Cask", "place:app_store_connect", "")
	b.Product.Surfaces = append(b.Product.Surfaces, Surface{ID: "source-path", Kind: "repo", Ref: "/workspace/products/cask"})
	store := whereStore(t, b, registryBundle("product:sundial", "Sundial"))
	got, err := Where(store, "/workspace/products/cask/ios-app/Sources")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProductID != "product:cask" || got.Status != WhereKnown {
		t.Fatalf("got %+v, want product:cask resolved by path", got)
	}
}

// TestWhereAllCoversEveryProductOnce is the portfolio view: each product appears
// exactly once however many spellings it has, with its place or unknown.
func TestWhereAllCoversEveryProductOnce(t *testing.T) {
	store := whereStore(t,
		registryBundle("product:tide-log", "Tide Log"),
		deployedBundle("product:tide_log", "Tide Log", "place:build-host", "make deploy"),
		registryBundle("product:sundial", "Sundial"),
	)
	all, err := WhereAll(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("groups = %d, want 2: %+v", len(all), all)
	}
	byID := map[string]WhereReport{}
	for _, r := range all {
		byID[r.ProductID] = r
	}
	if byID["product:tide_log"].Deployment.Place != "place:build-host" || byID["product:sundial"].Status != WhereUnknown {
		t.Fatalf("all = %+v", all)
	}
}

// TestRunWhereCLIJSONIsTheReport keeps the agent-facing JSON identical to the
// report the tests above pin, and makes an unresolved query a non-zero exit so
// a script cannot mistake "no such product" for "no deployment".
func TestRunWhereCLIJSONIsTheReport(t *testing.T) {
	store := whereStore(t, deployedBundle("product:tide_log", "Tide Log", "place:build-host", "make deploy"))
	var out, errOut bytes.Buffer
	if code := RunWhereCLI([]string{"tidelog", "--json"}, store, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	var got WhereReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out.String())
	}
	if got.Deployment.Command != "make deploy" {
		t.Fatalf("json deployment = %+v", got.Deployment)
	}
	out.Reset()
	if code := RunWhereCLI([]string{"nope"}, store, &out, &errOut); code != 1 {
		t.Fatalf("unresolved exit = %d, want 1", code)
	}
}

// TestWhereFoldsOnAppIdentityNotSharedURLs pins what counts as identity. An iOS
// app and its store page share a bundle id and are one product. A feed service
// that serves that product's API shares its URLs and is not — folding on URLs
// would hand the app's deploy command to the service, or the reverse.
func TestWhereFoldsOnAppIdentityNotSharedURLs(t *testing.T) {
	page := registryBundle("product:tidelog", "Tide Log")
	page.Product.Surfaces = []Surface{
		{ID: "app-store", Kind: "app_store", Ref: "appstore:com.example.tidelog"},
		{ID: "api", Kind: "web", Ref: "https://api.example.invalid/tidelog/status"},
	}
	app := deployedBundle("product:tidelog_ios", "Tide Log for iPhone", "place:app_store_connect", "")
	app.Product.Surfaces = []Surface{{ID: "bundle", Kind: "app_bundle", Ref: "bundle:com.example.tidelog"}}
	feeds := deployedBundle("product:feed-service", "Feed Service", "place:edge.example", "make deploy-feeds")
	feeds.Product.Surfaces = []Surface{{ID: "api", Kind: "web", Ref: "https://api.example.invalid/tidelog/status"}}
	store := whereStore(t, page, app, feeds)

	got, err := Where(store, "tidelog")
	if err != nil {
		t.Fatal(err)
	}
	if want := "product:tidelog,product:tidelog_ios"; strings.Join(got.Members, ",") != want {
		t.Fatalf("members = %v, want %s — the bundle id joins the app, the shared URL must not join the feed service", got.Members, want)
	}
	if got.Status != WhereKnown || got.Deployment.Place != "place:app_store_connect" {
		t.Fatalf("got %+v, want the app's place", got)
	}
	feed, err := Where(store, "feed-service")
	if err != nil {
		t.Fatal(err)
	}
	if feed.Deployment.Command != "make deploy-feeds" || len(feed.Members) != 1 {
		t.Fatalf("feed service = %+v, want it alone with its own command", feed)
	}
}

// TestWhereResolvesFromInsideAnImportedCheckout runs the real path an agent
// takes: a contract is imported from its checkout, then the agent asks from a
// subdirectory. The contract names a repo URL and an authority-host path on
// another machine, so only the contract's own directory can answer.
func TestWhereResolvesFromInsideAnImportedCheckout(t *testing.T) {
	contract := writeContract(t, "project.yaml", deployedContract)
	bundle, err := LoadProjectContractBundle(contract, "")
	if err != nil {
		t.Fatal(err)
	}
	store := whereStore(t, bundle, registryBundle("product:sundial", "Sundial"))
	got, err := Where(store, filepath.Join(filepath.Dir(contract), "platform", "src"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ProductID != bundle.Product.ID || got.Status != WhereKnown || got.Deployment.Command != "make deploy" {
		t.Fatalf("got %+v, want %s with its recorded command", got, bundle.Product.ID)
	}
}

// TestWhereListsSeparateTargetsWithoutCallingThemAConflict: an app repo and a
// web/API repo of one product record different places because they run in
// different places. Asking for the product lists both; asking for one target
// (by its id or from inside its checkout) selects it. Calling this a conflict
// would send an agent to "reconcile" two contracts that are both right.
func TestWhereListsSeparateTargetsWithoutCallingThemAConflict(t *testing.T) {
	web := deployedBundle("product:tide_log", "Tide Log", "place:edge.example", "make deploy-web")
	web.Product.Surfaces = append(web.Product.Surfaces, Surface{ID: "checkout", Kind: "repo", Ref: "/workspace/products/tide-log"})
	app := deployedBundle("product:tide_log_ios", "Tide Log iOS", "place:app_store_connect", "", "Tide Log")
	app.Product.Surfaces = append(app.Product.Surfaces, Surface{ID: "checkout", Kind: "repo", Ref: "/workspace/products/tide-log/ios-app"})
	store := whereStore(t, registryBundle("product:tidelog", "Tide Log"), web, app)

	whole, err := Where(store, "tidelog")
	if err != nil {
		t.Fatal(err)
	}
	if whole.Status != WhereMultiple || len(whole.Targets) != 2 || len(whole.Conflicts) != 0 {
		t.Fatalf("whole product = %+v, want two targets and no conflict", whole)
	}
	if whole.Deployment.Place != "" || whole.Deployment.Command != "" {
		t.Errorf("deployment = %+v, want empty until a target is named", whole.Deployment)
	}
	for query, want := range map[string]string{
		"product:tide_log_ios":                    "place:app_store_connect",
		"/workspace/products/tide-log/ios-app/UI": "place:app_store_connect",
		"product:tide_log":                        "place:edge.example",
	} {
		got, err := Where(store, query)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != WhereKnown || got.Deployment.Place != want {
			t.Errorf("where %q = %s %q, want known %q", query, got.Status, got.Deployment.Place, want)
		}
	}
}

// TestWherePathInsideAWorkspaceDoesNotSelectItsRootTarget: the web contract
// sits at the product root and the app contract in a subdirectory. A path in a
// third sibling (an Android app, a docs folder) is inside the root checkout, but
// handing it the root's deploy command would run the web deploy for an
// unrelated surface. The path must name the product and leave the choice open.
func TestWherePathInsideAWorkspaceDoesNotSelectItsRootTarget(t *testing.T) {
	web := deployedBundle("product:tide_log", "Tide Log", "place:edge.example", "make deploy-web")
	web.Product.Surfaces = append(web.Product.Surfaces, Surface{ID: "checkout", Kind: "repo", Ref: "/workspace/products/tide-log"})
	app := deployedBundle("product:tide_log_ios", "Tide Log iOS", "place:app_store_connect", "", "Tide Log")
	app.Product.Surfaces = append(app.Product.Surfaces, Surface{ID: "checkout", Kind: "repo", Ref: "/workspace/products/tide-log/ios-app"})
	store := whereStore(t, web, app)

	sibling, err := Where(store, "/workspace/products/tide-log/android-app/src")
	if err != nil {
		t.Fatal(err)
	}
	if sibling.Status != WhereMultiple || sibling.Deployment.Command != "" {
		t.Fatalf("sibling path = %s %+v, want multiple with no command selected", sibling.Status, sibling.Deployment)
	}
	inApp, err := Where(store, "/workspace/products/tide-log/ios-app/UI")
	if err != nil {
		t.Fatal(err)
	}
	if inApp.Status != WhereKnown || inApp.ProductID != "product:tide_log_ios" {
		t.Fatalf("app path = %s %s, want the app target", inApp.Status, inApp.ProductID)
	}
}

// TestWhereContractsSharingARepositoryAreSeparateTargets: an app contract at a
// repository's root and a landing contract in its subfolder share one remote.
// Their checkouts differ, so they are two targets — not one target whose two
// records disagree.
func TestWhereContractsSharingARepositoryAreSeparateTargets(t *testing.T) {
	app := deployedBundle("product:tide_log", "Tide Log", "place:app_store_connect", "")
	landing := deployedBundle("product:tide_log_landing", "Tide Log Landing", "place:edge.example", "", "Tide Log")
	app.Product.Surfaces = append(app.Product.Surfaces,
		Surface{ID: "source-repo", Kind: "repo", Ref: "git@example.invalid:owner/tide-log.git"},
		Surface{ID: "checkout", Kind: "repo", Ref: "/workspace/products/tide-log"})
	landing.Product.Surfaces = append(landing.Product.Surfaces,
		Surface{ID: "source-repo", Kind: "repo", Ref: "git@example.invalid:owner/tide-log.git"},
		Surface{ID: "checkout", Kind: "repo", Ref: "/workspace/products/tide-log/landing"})
	store := whereStore(t, app, landing)
	got, err := Where(store, "tidelog")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != WhereMultiple || len(got.Targets) != 2 {
		t.Fatalf("got %s with %d targets (%v), want multiple with 2", got.Status, len(got.Targets), got.Conflicts)
	}
}
