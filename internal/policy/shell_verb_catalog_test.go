package policy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// catalogTestHome points the classifier at an empty, disposable COMPOUND_HOME so
// a live operator extension file can never leak into a test, and returns it so a
// test can install its own extension file.
func catalogTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("COMPOUND_HOME", home)
	return home
}

func classifyCatalogCommand(command string) ActionClassification {
	return ClassifyToolAction(monitorRequest("Bash", map[string]any{"command": command}))
}

func TestShellVerbCatalogBuiltinRows(t *testing.T) {
	catalogTestHome(t)
	cases := []struct {
		command       string
		capability    string
		mutationClass string
		destination   string
		row           string
	}{
		// Gmail through gog: reads and sends are different capabilities, and the
		// account a grant is scoped to is the destination.
		{"gog gmail search 'from:apple' --account ops@example.com", "gmail.message.read", "read", "gmail-account:ops@example.com", "gog-gmail-read"},
		{"gog -a founder@example.com gmail get 18c2", "gmail.message.read", "read", "gmail-account:founder@example.com", "gog-gmail-read"},
		{"gog gmail thread get t1 --account=ops@example.com", "gmail.message.read", "read", "gmail-account:ops@example.com", "gog-gmail-thread-read"},
		{"gog gmail messages search is:unread -a ops@example.com", "gmail.message.read", "read", "gmail-account:ops@example.com", "gog-gmail-messages-read"},
		{"gog gmail search foo", "gmail.message.read", "read", "gmail-account:unresolved", "gog-gmail-read"},
		{"gog gmail send --to x@example.com --subject hi --body yo -a ops@example.com", "gmail.message.send", "public", "gmail-account:ops@example.com", "gog-gmail-send"},
		{"gog gmail forward --to x@example.com m1 --account ops@example.com", "gmail.message.send", "public", "gmail-account:ops@example.com", "gog-gmail-send"},
		{"gog gmail drafts send d1 --account ops@example.com", "gmail.message.send", "public", "gmail-account:ops@example.com", "gog-gmail-drafts-send"},
		// --gmail-no-send makes gog itself refuse every send, so a send-family
		// command under it cannot send and keeps the read classification.
		{"gog gmail send --to x@example.com --gmail-no-send -a ops@example.com", "gmail.message.read", "read", "gmail-account:ops@example.com", "gog-gmail-no-send"},
		{"gog --gmail-no-send gmail drafts send d1", "gmail.message.read", "read", "gmail-account:unresolved", "gog-gmail-drafts-no-send"},
		{"gog gmail search foo --gmail-no-send -a ops@example.com", "gmail.message.read", "read", "gmail-account:ops@example.com", "gog-gmail-read"},
		// Only the bare flag (or =true) blocks sending; after `--` it is an argument.
		{"gog gmail send --to x@example.com --gmail-no-send=false", "gmail.message.send", "public", "gmail-account:unresolved", "gog-gmail-send"},
		{"gog gmail send --gmail-no-send --gmail-no-send=false", "gmail.message.send", "public", "gmail-account:unresolved", "gog-gmail-send"},
		{"gog gmail -- send --gmail-no-send", "gmail.message.send", "public", "gmail-account:unresolved", "gog-gmail-send"},
		// An env prefix can change which account gog defaults to.
		{"GOG_ACCOUNT=other@example.com gog gmail search foo", "gmail.message.read", "read", "gmail-account:unresolved", "gog-gmail-read"},
		// App Store Connect through asc.
		{"asc reviews list --app app-fixture-1", "app_store.customer_reviews.read", "read", "asc-app:app-fixture-1", "asc-reviews-read"},
		{"asc reviews --app app-fixture-1 --stars 1 --sort -createdDate", "app_store.customer_reviews.read", "read", "asc-app:app-fixture-1", "asc-reviews-list-default"},
		{"asc reviews ratings --app=app-fixture-1", "app_store.customer_reviews.read", "read", "asc-app:app-fixture-1", "asc-reviews-read"},
		{"asc reviews response for-review --review-id r1", "app_store.customer_reviews.read", "read", "asc-app:unresolved", "asc-reviews-response-read"},
		{"asc reviews respond --review-id r1 --response 'Thanks!'", "app_store.review_response.write", "external_side_effect", "asc-app:unresolved", "asc-reviews-respond"},
		{"asc reviews --app app-fixture-1 respond --review-id r1 --response hi", "app_store.review_response.write", "external_side_effect", "asc-app:app-fixture-1", "asc-reviews-respond"},
		{"asc reviews respond-batch --app app-fixture-1 --file replies.json --confirm", "app_store.review_response.write", "external_side_effect", "asc-app:app-fixture-1", "asc-reviews-respond-batch"},
		{"asc reviews response delete --id resp1 --confirm", "app_store.delete", "destructive", "asc-app:unresolved", "asc-delete-nested"},
		{"asc versions delete --app app-fixture-1 --version-id v1", "app_store.delete", "destructive", "asc-app:app-fixture-1", "asc-delete"},
		{"asc versions list --app app-fixture-1", "app_store.read", "read", "asc-app:app-fixture-1", "asc-versions-read"},
		{"asc apps list", "app_store.read", "read", "asc-app:unresolved", "asc-apps-read"},
		// Paddle through an HTTP client: GET is a read, any other method a write,
		// and the destination is the API host, not the command text.
		{`curl -s -H "Authorization: Bearer $PADDLE_API_KEY" https://api.paddle.com/transactions`, "paddle.api.read", "read", "paddle-api:api.paddle.com", "paddle-api-read"},
		{"curl -sS 'https://api.paddle.com/subscriptions?status=active'", "paddle.api.read", "read", "paddle-api:api.paddle.com", "paddle-api-read"},
		{"curl -X GET https://api.paddle.com/customers", "paddle.api.read", "read", "paddle-api:api.paddle.com", "paddle-api-read"},
		{"curl -I https://api.paddle.com/", "paddle.api.read", "read", "paddle-api:api.paddle.com", "paddle-api-read"},
		{"http GET https://api.paddle.com/transactions Authorization:'Bearer x'", "paddle.api.read", "read", "paddle-api:api.paddle.com", "paddle-api-read"},
		{"xh https://api.paddle.com/transactions status==completed", "paddle.api.read", "read", "paddle-api:api.paddle.com", "paddle-api-read"},
		{"curl -X POST https://api.paddle.com/adjustments -d @refund.json", "paddle.api.write", "external_side_effect", "paddle-api:api.paddle.com", "paddle-api-write"},
		{"curl -XPATCH https://api.paddle.com/prices/pri_1", "paddle.api.write", "external_side_effect", "paddle-api:api.paddle.com", "paddle-api-write"},
		{"curl https://api.paddle.com/adjustments --data '{\"x\":1}'", "paddle.api.write", "external_side_effect", "paddle-api:api.paddle.com", "paddle-api-write"},
		{"curl https://api.paddle.com/transactions -o tx.json", "paddle.api.write", "external_side_effect", "paddle-api:api.paddle.com", "paddle-api-write"},
		{"http POST https://api.paddle.com/adjustments action=refund", "paddle.api.write", "external_side_effect", "paddle-api:api.paddle.com", "paddle-api-write"},
		{"xh https://api.paddle.com/adjustments action=refund", "paddle.api.write", "external_side_effect", "paddle-api:api.paddle.com", "paddle-api-write"},
		// Compound's own ads projection: reading the report vs writing the ledger.
		{"compound product ads", "ads.report.read", "read", "ads-product:all", "compound-product-ads-read"},
		{"compound product ads --json --window 7d --product demo-app", "ads.report.read", "read", "ads-product:demo-app", "compound-product-ads-read"},
		{"compound product ads platforms --json", "ads.report.read", "read", "ads-product:all", "compound-product-ads-subcommand-read"},
		{"compound product ads import --platform apple_search_ads --product demo-app --report r.csv", "ads.report.write", "local_reversible", "ads-product:demo-app", "compound-product-ads-write"},
		{"compound product ads signal --platform apple_search_ads --product demo-app --before 7d --after 7d", "ads.report.write", "local_reversible", "ads-product:demo-app", "compound-product-ads-write"},
		// Wrappers and absolute system paths resolve to the same verb.
		{"timeout 60 /opt/homebrew/bin/gog gmail search foo -a ops@example.com", "gmail.message.read", "read", "gmail-account:ops@example.com", "gog-gmail-read"},
	}
	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			got := classifyCatalogCommand(tc.command)
			if !got.Known {
				t.Fatalf("catalog row must classify as known: %+v", got)
			}
			if got.Domain != ActionDomainShell {
				t.Fatalf("domain = %q, want shell (the catalog names the capability, not a new domain)", got.Domain)
			}
			if gotCap := actionCapability(got); gotCap != tc.capability {
				t.Fatalf("capability = %q, want %q (%+v)", gotCap, tc.capability, got)
			}
			if got.MutationClass != tc.mutationClass {
				t.Fatalf("mutation_class = %q, want %q", got.MutationClass, tc.mutationClass)
			}
			if got.Destination != tc.destination {
				t.Fatalf("destination = %q, want %q", got.Destination, tc.destination)
			}
			if got.CatalogRow != tc.row {
				t.Fatalf("catalog row = %q, want %q", got.CatalogRow, tc.row)
			}
			wantEvidence := "command:" + actionHash([]byte(tc.command))
			if len(got.EvidenceRefs) != 1 || got.EvidenceRefs[0] != wantEvidence {
				t.Fatalf("evidence refs = %v, want the command hash %q", got.EvidenceRefs, wantEvidence)
			}
		})
	}
}

// TestShellVerbCatalogGoldenFallbackUnchanged pins every command the catalog
// must NOT claim -- unrelated commands, unknown verbs, and near-misses of
// catalog rows -- to the classification main produced before the catalog
// existed (testdata generated from 822e0b69d).
func TestShellVerbCatalogGoldenFallbackUnchanged(t *testing.T) {
	catalogTestHome(t)
	body, err := os.ReadFile(filepath.Join("testdata", "shell_verb_catalog_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Command        string               `json:"command"`
		Classification ActionClassification `json:"classification"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) < 40 {
		t.Fatalf("golden fixture has %d rows; it was generated with 46", len(rows))
	}
	for _, row := range rows {
		got := classifyCatalogCommand(row.Command)
		if !reflect.DeepEqual(got, row.Classification) {
			t.Errorf("%q drifted from main:\n got  %+v\n want %+v", row.Command, got, row.Classification)
		}
	}
}

func TestShellVerbCatalogChainTakesMostSevereClass(t *testing.T) {
	catalogTestHome(t)
	cases := []struct {
		command       string
		capability    string
		mutationClass string
		destination   string
	}{
		// A read row piped into a local reader keeps the catalog capability; the
		// local segment's class is the more severe one.
		{"asc reviews list --app 1 | jq '.data[]'", "app_store.customer_reviews.read", "local_reversible", "asc-app:1"},
		{"gog gmail search foo -a a@example.com | head -5", "gmail.message.read", "local_reversible", "gmail-account:a@example.com"},
		// Two catalog rows: the most severe decides, not the first.
		{"asc reviews list --app 1 && gog gmail send --to x@example.com -a a@example.com", "gmail.message.send", "public", "gmail-account:a@example.com"},
		{"curl https://api.paddle.com/transactions | jq . && asc reviews respond --review-id r --response hi", "app_store.review_response.write", "external_side_effect", "asc-app:unresolved"},
		// Equal severity: the first catalog capability wins.
		{"gog gmail search a -a one@example.com; asc reviews list --app 1", "gmail.message.read", "read", "gmail-account:one@example.com"},
	}
	for _, tc := range cases {
		got := classifyCatalogCommand(tc.command)
		if actionCapability(got) != tc.capability || got.MutationClass != tc.mutationClass || got.Destination != tc.destination || !got.Known {
			t.Errorf("%q = capability %q class %q destination %q known %t; want %q %q %q",
				tc.command, actionCapability(got), got.MutationClass, got.Destination, got.Known,
				tc.capability, tc.mutationClass, tc.destination)
		}
	}
	// A catalog row never launders a neighbour the catalog does not know: the
	// chain falls back to exactly what main computes for it.
	for _, command := range []string{
		"gog gmail send --to x@example.com && curl -X POST https://example.com/x",
		"asc reviews list --app 1 | sh",
		"gog gmail search foo && frobnicate",
	} {
		got := classifyCatalogCommand(command)
		if got.CatalogRow != "" || got.Capability != "" {
			t.Errorf("%q must fall back to the legacy classifier, got catalog row %q", command, got.CatalogRow)
		}
	}
}

func TestShellVerbCatalogExtensionFileAddsRow(t *testing.T) {
	home := catalogTestHome(t)
	writeCatalogExtension(t, home, `schema_version: compound.shell_verb_catalog.v0.1
rows:
  - id: stripe-api-read
    binary: curl
    url_host: api.stripe.com
    methods: [GET, HEAD]
    capability: stripe.api.read
    mutation_class: read
    read_only: true
    destination_kind: stripe-api
  - id: kit-broadcast-send
    binary: kit
    pattern: broadcasts send
    capability: kit.broadcast.send
    mutation_class: public
    blast_radius: newsletter_list
    destination_kind: kit-list
    destination_flags: [--list]
  - id: gog-gmail-read
    binary: gog
    pattern: gmail search
    value_flags: [--account, -a]
    capability: gmail.search.operator_override
    mutation_class: read
    destination_kind: gmail-account
    destination_flags: [--account, -a]
`)
	stripe := classifyCatalogCommand("curl https://api.stripe.com/v1/charges")
	if actionCapability(stripe) != "stripe.api.read" || stripe.Destination != "stripe-api:api.stripe.com" || stripe.MutationClass != "read" {
		t.Fatalf("extension HTTP row not applied: %+v", stripe)
	}
	kit := classifyCatalogCommand("kit broadcasts send --list vip --subject hi")
	if actionCapability(kit) != "kit.broadcast.send" || kit.Destination != "kit-list:vip" ||
		kit.MutationClass != "public" || kit.BlastRadius != "newsletter_list" || kit.Reversibility != "hard" {
		t.Fatalf("extension verb row not applied: %+v", kit)
	}
	// Same id as a built-in: the operator row replaces it.
	override := classifyCatalogCommand("gog gmail search foo -a a@example.com")
	if actionCapability(override) != "gmail.search.operator_override" {
		t.Fatalf("extension row must replace the built-in with the same id, got %q", actionCapability(override))
	}
	// Built-ins the file does not touch keep working.
	if got := actionCapability(classifyCatalogCommand("asc reviews list --app 1")); got != "app_store.customer_reviews.read" {
		t.Fatalf("built-in row lost after merging the extension: %q", got)
	}
	catalog, source, err := LoadShellVerbCatalog(home)
	if err != nil {
		t.Fatal(err)
	}
	if source != ShellVerbCatalogPath(home) || catalog.ExtensionRows() != 3 {
		t.Fatalf("source=%q extension rows=%d", source, catalog.ExtensionRows())
	}
}

// TestShellVerbCatalogStrictFlagsRefuseUnlistedFlagsAnywhere pins strict_flags.
// A plain row tolerates an unlisted flag after its pattern words, and any
// --flag=value, so `doorcli click @e1 --state f.json` would ride a
// local_reversible row. A strict row refuses those and a VAR=value prefix,
// and the command keeps the fail-closed fallback.
func TestShellVerbCatalogStrictFlagsRefuseUnlistedFlagsAnywhere(t *testing.T) {
	home := catalogTestHome(t)
	writeCatalogExtension(t, home, `schema_version: compound.shell_verb_catalog.v0.1
rows:
  - id: doorcli-act-strict
    binary: doorcli
    pattern: click
    value_flags: [--session]
    strict_flags: true
    capability: browser.isolated.act
    mutation_class: local_reversible
    destination_kind: isolated-browser
  - id: loosecli-act
    binary: loosecli
    pattern: click
    value_flags: [--session]
    capability: browser.isolated.act
    mutation_class: local_reversible
    destination_kind: isolated-browser
`)
	for _, command := range []string{"doorcli click @e1", "doorcli --session s1 click @e1", "doorcli click @e1 --session=s1"} {
		if got := classifyCatalogCommand(command); got.CatalogRow != "doorcli-act-strict" {
			t.Fatalf("%q: listed flags must keep the strict row, got %q", command, got.CatalogRow)
		}
	}
	for _, command := range []string{
		"doorcli click @e1 --state f.json",
		"doorcli --state=f.json click @e1",
		"doorcli click --headed @e1",
		"DOOR_STATE=f.json doorcli click @e1",
	} {
		if got := classifyCatalogCommand(command); got.CatalogRow != "" || got.Known {
			t.Fatalf("%q: an unlisted flag or env prefix must leave the strict row, got row %q class %q", command, got.CatalogRow, got.MutationClass)
		}
	}
	// Control: the same shapes on a plain row still match, so the assertions
	// above fail if strict_flags stops being read.
	for _, command := range []string{"loosecli click @e1 --state f.json", "loosecli --state=f.json click @e1"} {
		if got := classifyCatalogCommand(command); got.CatalogRow != "loosecli-act" {
			t.Fatalf("%q: plain control row must match, got %q", command, got.CatalogRow)
		}
	}
}

func TestShellVerbCatalogExtensionInvalidRowIsNamedAndRejected(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"bad mutation class", `rows:
  - id: good-row
    binary: kit
    pattern: list
    capability: kit.list.read
    mutation_class: read
    destination_kind: kit
  - id: typo-row
    binary: kit
    pattern: send
    capability: kit.broadcast.send
    mutation_class: publik
    destination_kind: kit
`, []string{`row 2`, `"typo-row"`, `mutation_class "publik"`}},
		{"unknown field", `rows:
  - id: field-typo
    binary: kit
    patern: send
    capability: kit.send
    mutation_class: public
    destination_kind: kit
`, []string{"patern"}},
		{"reserved binary", `rows:
  - id: bash-read
    binary: bash
    capability: shell.bash.read
    mutation_class: read
    destination_kind: bash
`, []string{`"bash-read"`, `binary "bash"`}},
		{"missing destination", `rows:
  - id: no-dest
    binary: kit
    capability: kit.send
    mutation_class: public
`, []string{`"no-dest"`, "destination_kind"}},
		{"read_only contradicts class", `rows:
  - id: liar
    binary: kit
    pattern: send
    capability: kit.send
    mutation_class: public
    read_only: true
    destination_kind: kit
`, []string{`"liar"`, "read_only"}},
		{"url_host on a non-HTTP binary", `rows:
  - id: host-misuse
    binary: kit
    url_host: api.kit.com
    methods: [GET]
    capability: kit.api.read
    mutation_class: read
    destination_kind: kit-api
`, []string{`"host-misuse"`, "url_host"}},
		{"flag listed with two arities", `rows:
  - id: arity
    binary: kit
    pattern: send
    value_flags: [--list]
    bool_flags: [--list]
    capability: kit.send
    mutation_class: public
    destination_kind: kit
`, []string{`"arity"`, "--list"}},
		{"duplicate id", `rows:
  - id: twice
    binary: kit
    capability: kit.a
    mutation_class: read
    destination_kind: kit
  - id: twice
    binary: kit
    capability: kit.b
    mutation_class: read
    destination_kind: kit
`, []string{`row 2`, `"twice"`, "duplicate"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := catalogTestHome(t)
			writeCatalogExtension(t, home, tc.body)
			_, _, err := LoadShellVerbCatalog(home)
			if err == nil {
				t.Fatal("invalid extension file must be rejected with an error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err, want)
				}
			}
			// The rejected file teaches nothing -- not even its valid rows -- while
			// the built-ins keep classifying.
			if got := classifyCatalogCommand("kit list"); got.CatalogRow != "" {
				t.Fatalf("a row from a rejected file was applied: %+v", got)
			}
			if got := actionCapability(classifyCatalogCommand("gog gmail search foo")); got != "gmail.message.read" {
				t.Fatalf("built-ins must survive a rejected extension, got %q", got)
			}
		})
	}
}

func TestShellVerbCatalogMonitorBindsSemanticDestinationForGrants(t *testing.T) {
	catalogTestHome(t)
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{
		{ID: "grant_gmail_read", Capability: "gmail.message.read", MutationClass: "read", Destination: "gmail-account:ops@example.com"},
		{ID: "grant_review_response", Capability: "app_store.review_response.write", MutationClass: "external_side_effect",
			ReversibilityCeiling: "reversible", BlastRadius: "one_review_response"},
	}
	command := "gog gmail search 'from:apple' -a ops@example.com"
	read := MonitorToolAction(monitorRequest("Bash", map[string]any{"command": command}), cfg, fixedNow)
	if read.Intent.Capability != "gmail.message.read" || read.Intent.Destination != "gmail-account:ops@example.com" {
		t.Fatalf("intent capability/destination = %q/%q", read.Intent.Capability, read.Intent.Destination)
	}
	if read.Decision.Outcome != "allow" || read.Decision.StandingGrantID != "grant_gmail_read" {
		t.Fatalf("gmail read must ride its standing grant, got %s (%s) %v", read.Decision.Outcome, read.Decision.StandingGrantID, read.Decision.Reasons)
	}
	hash := "command:" + actionHash([]byte(command))
	if !containsString(read.Intent.EvidenceRefs, hash) {
		t.Fatalf("command hash %q must survive as evidence, got %v", hash, read.Intent.EvidenceRefs)
	}
	if err := read.Intent.Validate(); err != nil {
		t.Fatalf("catalog intent must validate: %v", err)
	}

	respond := MonitorToolAction(monitorRequest("Bash", map[string]any{"command": "asc reviews respond --review-id r1 --response 'Thanks for the note!'"}), cfg, fixedNow)
	if respond.Decision.Outcome != "allow_with_obligations" || respond.Decision.StandingGrantID != "grant_review_response" {
		t.Fatalf("one review response must ride its grant, got %s %v", respond.Decision.Outcome, respond.Decision.Reasons)
	}
	batch := MonitorToolAction(monitorRequest("Bash", map[string]any{"command": "asc reviews respond-batch --app 1 --file r.json --confirm"}), cfg, fixedNow)
	if Allowed(batch.Decision) {
		t.Fatalf("a batch response is wider than a one-response grant and must hold, got %s", batch.Decision.Outcome)
	}
	// The grant for reads must not open sends from the same account.
	send := MonitorToolAction(monitorRequest("Bash", map[string]any{"command": "gog gmail send --to x@example.com -a ops@example.com"}), cfg, fixedNow)
	if Allowed(send.Decision) {
		t.Fatalf("gmail send must not ride the read grant, got %s", send.Decision.Outcome)
	}
	// A different account is a different destination.
	other := MonitorToolAction(monitorRequest("Bash", map[string]any{"command": "gog gmail search x -a someone@example.com"}), cfg, fixedNow)
	if other.Decision.StandingGrantID != "" {
		t.Fatalf("grant for one account matched another: %s", other.Decision.StandingGrantID)
	}
}

func TestRunClassifyCLIReportsCatalogRowOrFallback(t *testing.T) {
	home := catalogTestHome(t)
	var stdout, stderr bytes.Buffer
	code := RunClassifyCLI([]string{"--command", "gog gmail send --to x@example.com -a ops@example.com"}, FirstPartyCommandCatalog{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, want := range []string{
		"capability: gmail.message.send",
		"mutation_class: public",
		"destination: gmail-account:ops@example.com",
		"catalog_row: gog-gmail-send",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("text output missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	code = RunClassifyCLI([]string{"--command=ls -la", "--json"}, FirstPartyCommandCatalog{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var report ShellClassifyReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("json output: %v\n%s", err, stdout.String())
	}
	if report.CatalogRow != "fallback" || report.Capability != "shell.local_command" || report.MutationClass != "local_reversible" ||
		!strings.HasPrefix(report.Destination, "command:sha256:") {
		t.Fatalf("fallback report = %+v", report)
	}

	stdout.Reset()
	stderr.Reset()
	if code := RunClassifyCLI([]string{"--json"}, FirstPartyCommandCatalog{}, &stdout, &stderr); code != 2 {
		t.Fatalf("missing --command must be a usage error, got %d", code)
	}

	writeCatalogExtension(t, home, "rows:\n  - id: broken\n    binary: kit\n")
	stdout.Reset()
	stderr.Reset()
	code = RunClassifyCLI([]string{"--command", "gog gmail search foo"}, FirstPartyCommandCatalog{}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), `"broken"`) {
		t.Fatalf("a rejected extension must fail the command and name the row: exit %d stderr %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "catalog_row: gog-gmail-read") {
		t.Fatalf("classification still prints from the built-ins:\n%s", stdout.String())
	}
}

func writeCatalogExtension(t *testing.T, home, body string) {
	t.Helper()
	path := ShellVerbCatalogPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestShellHTTPHostUserinfoCannotSpoofPaddle builds the hostile URL by
// concatenation so the source never carries an email-shaped literal: a
// trusted host placed before `@` lands in userinfo and must not match the
// Paddle rows.
func TestShellHTTPHostUserinfoCannotSpoofPaddle(t *testing.T) {
	catalogTestHome(t)
	hostile := "https://api.paddle.com" + "@" + "evil.example.invalid/x"
	host, ok := shellHTTPHost(hostile)
	if !ok || host != "evil.example.invalid" {
		t.Fatalf("shellHTTPHost(%q) = %q, %t; want evil.example.invalid, true", hostile, host, ok)
	}
	got := classifyCatalogCommand("curl " + hostile)
	if cap := actionCapability(got); strings.HasPrefix(cap, "paddle.") {
		t.Fatalf("userinfo spoof classified as %q; want the network fallback", cap)
	}
	if got.Operation != "network_command" {
		t.Fatalf("userinfo spoof operation = %q; want network_command", got.Operation)
	}
}
