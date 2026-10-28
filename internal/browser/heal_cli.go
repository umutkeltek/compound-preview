package browser

// heal_cli.go — argument parsing for the self-heal verbs. They exit with
// their own codes (probe/heal: 0 ok, 3 needs attention, 2 error; run: 0 ok,
// 1 a step failed, 2 error), so the dispatcher returns a code instead of an
// error.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"compound/internal/home"
)

const selfHealUsage = "       compound browser probe --profile ID [--json] [--home DIR]   exit 0 logged in, 3 logged out, 2 error\n" +
	"       compound browser run --profile ID --recipe NAME [--param k=v ...] [--json] [--home DIR]   exit 0 ok, 1 step failed, 2 error\n" +
	"       compound browser heal --profile ID [--json] [--home DIR]   exit 0 logged in, 3 needs operator (see return-queue.jsonl), 2 error\n" +
	"       compound browser recipes list [--profile ID] [--json] [--home DIR]\n" +
	"       compound browser recipes show --profile ID --recipe NAME [--json] [--home DIR]"

type selfHealArgs struct {
	verb, sub, home, profile, recipe string
	json                             bool
	params                           map[string]string
}

func parseSelfHealArgs(args []string) (selfHealArgs, error) {
	a := selfHealArgs{verb: args[0], params: map[string]string{}}
	rest := args[1:]
	if a.verb == "recipes" {
		if len(rest) == 0 || (rest[0] != "list" && rest[0] != "show") {
			return a, fmt.Errorf("recipes needs list or show")
		}
		a.sub, rest = rest[0], rest[1:]
	}
	seen := map[string]bool{}
	for i := 0; i < len(rest); i++ {
		flag := rest[i]
		switch flag {
		case "--json":
			a.json = true
			continue
		case "--home", "--profile", "--recipe", "--param":
		default:
			return a, fmt.Errorf("unknown browser %s argument %q", a.verb, flag)
		}
		if i+1 >= len(rest) || strings.HasPrefix(rest[i+1], "--") {
			return a, fmt.Errorf("%s requires one value", flag)
		}
		i++
		value := rest[i]
		if flag != "--param" {
			if seen[flag] {
				return a, fmt.Errorf("duplicate flag %s", flag)
			}
			seen[flag] = true
		}
		switch flag {
		case "--home":
			a.home = value
		case "--profile":
			a.profile = value
		case "--recipe":
			a.recipe = value
		case "--param":
			k, v, ok := strings.Cut(value, "=")
			if !ok || strings.TrimSpace(k) == "" || strings.HasPrefix(k, "secret:") {
				return a, fmt.Errorf("--param needs key=value (secrets come from the credential provider)")
			}
			if _, dup := a.params[k]; dup {
				return a, fmt.Errorf("duplicate --param %s", k)
			}
			a.params[k] = v
		}
	}
	needsRecipe := a.verb == "run" || a.sub == "show"
	if a.recipe != "" && !needsRecipe {
		return a, fmt.Errorf("--recipe is a run or recipes show option")
	}
	if len(a.params) > 0 && a.verb != "run" {
		return a, fmt.Errorf("--param is a run option")
	}
	if a.profile == "" && a.sub != "list" {
		return a, fmt.Errorf("browser %s requires --profile", a.verb)
	}
	if a.profile != "" {
		if err := validSelfHealID("profile", a.profile); err != nil {
			return a, err
		}
	}
	if needsRecipe {
		if a.recipe == "" {
			return a, fmt.Errorf("browser %s requires --recipe", a.verb)
		}
		if err := validRecipeName(a.recipe); err != nil {
			return a, err
		}
	}
	return a, nil
}

// runSelfHealCLI handles probe, run, heal and recipes; any other verb is
// left to the existing browser dispatchers.
func runSelfHealCLI(args []string, out, errOut io.Writer) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "probe", "run", "heal", "recipes":
	default:
		return false, 0
	}
	a, err := parseSelfHealArgs(args)
	if err != nil {
		fmt.Fprintln(errOut, "error:", err)
		fmt.Fprintln(errOut, "usage:"+strings.TrimPrefix(selfHealUsage, "      "))
		return true, 2
	}
	homePath, err := home.Resolve(a.home)
	if err != nil {
		fmt.Fprintln(errOut, "error:", err)
		return true, 2
	}
	ctx := context.Background()
	switch a.verb {
	case "probe":
		res, code := runProbe(ctx, homePath, a.profile)
		emitSelfHeal(out, errOut, a.json, res, res.Error, func() string {
			return fmt.Sprintf("browser probe profile=%s logged_in=%t evidence=%s", res.Profile, res.LoggedIn, res.EvidencePath)
		})
		return true, code
	case "heal":
		res, code := runHeal(ctx, homePath, a.profile)
		emitSelfHeal(out, errOut, a.json, res, res.Error, func() string {
			return fmt.Sprintf("browser heal profile=%s status=%s reason=%s attempts_6h=%d queued=%t", res.Profile, res.Status, res.Reason, res.AttemptsInWindow, res.Queued)
		})
		return true, code
	case "run":
		return true, cmdSelfHealRun(ctx, homePath, a, out, errOut)
	}
	return true, cmdSelfHealRecipes(homePath, a, out, errOut)
}

func emitSelfHeal(out, errOut io.Writer, asJSON bool, v any, errText string, text func() string) {
	if asJSON {
		_ = json.NewEncoder(out).Encode(v)
	} else if errText == "" {
		fmt.Fprintln(out, text())
	}
	if errText != "" {
		fmt.Fprintln(errOut, "error:", errText)
	}
}

func cmdSelfHealRun(ctx context.Context, homePath string, a selfHealArgs, out, errOut io.Writer) int {
	refuse := func(err error) int {
		if a.json {
			_ = json.NewEncoder(out).Encode(map[string]string{"status": "error", "reason": reasonOf(err), "error": err.Error()})
		}
		fmt.Fprintln(errOut, "error:", err)
		return 2
	}
	p, err := loadSelfHealProfile(homePath, a.profile)
	if err != nil {
		return refuse(err)
	}
	recipe, err := loadRecipe(homePath, a.profile, a.recipe)
	if err != nil {
		return refuse(err)
	}
	params, secretFields := recipeRequirements(recipe)
	var missing []string
	for _, name := range params {
		if _, ok := a.params[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return refuse(fmt.Errorf("missing_param: recipe needs --param %s", strings.Join(missing, ", --param ")))
	}
	if !profileEnrolled(p) {
		return refuse(fmt.Errorf("profile_not_enrolled: user_data_dir does not exist"))
	}
	var secrets secretResolver
	if len(secretFields) > 0 {
		provider, err := newCredentialProvider(homePath, p)
		if err != nil {
			return refuse(err)
		}
		secrets = provider.resolve
	}
	lock, err := lockProfile(homePath, a.profile)
	if err != nil {
		return refuse(err)
	}
	defer unlockSelfHealFile(lock)
	client, err := newAgentBrowserClient(homePath, p)
	if err != nil {
		return refuse(err)
	}
	defer client.close()
	receipt, err := executeRecipe(ctx, homePath, client, recipe, a.params, secrets, runPurposeRun)
	if err != nil {
		return refuse(err)
	}
	if a.json {
		_ = json.NewEncoder(out).Encode(receipt)
	} else {
		fmt.Fprintf(out, "browser run %s profile=%s recipe=%s status=%s healed=%d recipe_updated=%t\n", receipt.ID, receipt.Profile, receipt.Recipe, receipt.Status, receipt.HealedSteps, receipt.RecipeUpdated)
		for _, s := range receipt.Steps {
			fmt.Fprintf(out, "- %d %s %s ref=%s->%s %dms %s\n", s.Index, s.Action, s.Outcome, s.RefBefore, s.RefAfter, s.DurationMS, s.Error)
		}
	}
	if receipt.Status != "ok" {
		return 1
	}
	return 0
}

func cmdSelfHealRecipes(homePath string, a selfHealArgs, out, errOut io.Writer) int {
	if a.sub == "show" {
		recipe, err := loadRecipe(homePath, a.profile, a.recipe)
		if err != nil {
			fmt.Fprintln(errOut, "error:", err)
			return 2
		}
		if a.json {
			_ = json.NewEncoder(out).Encode(recipe)
			return 0
		}
		fmt.Fprintf(out, "recipe %s/%s revision=%d steps=%d\n", recipe.Profile, recipe.Name, recipe.Revision, len(recipe.Steps))
		for i, s := range recipe.Steps {
			d := s.Descriptor
			fmt.Fprintf(out, "- %d %s role=%q name=%q text=%q placeholder=%q url=%q cached_ref=%s\n", i, s.Action, d.Role, d.Name, d.Text, d.Placeholder, d.URL, s.CachedRef)
		}
		return 0
	}
	list, err := listRecipes(homePath, a.profile)
	if err != nil {
		fmt.Fprintln(errOut, "error:", err)
		return 2
	}
	if a.json {
		_ = json.NewEncoder(out).Encode(list)
		return 0
	}
	for _, r := range list {
		fmt.Fprintf(out, "%s/%s steps=%d revision=%d updated_at=%s\n", r.Profile, r.Name, r.Steps, r.Revision, r.UpdatedAt)
	}
	return 0
}
