package browser

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/work/executewire"
)

const portableBrowserUsage = "usage: compound browser settings [--json] [--home DIR] [--enable|--disable --work work_ID]\n" +
	"       compound browser route [--json] [--home DIR] [--profile ID]\n" +
	"       compound browser provider-pricing --work work_ID --model jev-1.13.0 --max-usd-per-request 0.003 [--json] [--home DIR]\n" +
	"       compound browser credential-set --work work_ID --confirm [--json] [--home DIR] < KEY_ON_STDIN\n" +
	"       compound browser credential-rotate --work work_ID --confirm [--json] [--home DIR] < KEY_ON_STDIN\n" +
	"       compound browser goal --work work_ID --actor ACTOR --url HTTPS_URL --goal TEXT --expect-text TEXT [--provider-budget-usd USD] [--max-steps 1..6] [--profile ID] [--json] [--home DIR] (budget required for Jev)"

func runPortableBrowser(args []string, out io.Writer) (bool, error) {
	return runPortableBrowserInput(args, out, os.Stdin)
}

func runPortableBrowserInput(args []string, out io.Writer, in io.Reader) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	verb := args[0]
	if verb != "settings" && verb != "route" && verb != "goal" && verb != "provider-pricing" && verb != "credential-set" && verb != "credential-rotate" {
		return false, nil
	}
	values := map[string]string{}
	switches := map[string]bool{}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--json", "--enable", "--disable", "--confirm":
			if switches[arg] {
				return true, fmt.Errorf("duplicate flag %s", arg)
			}
			switches[arg] = true
		case "--home", "--work", "--actor", "--profile", "--url", "--goal", "--expect-text", "--max-steps", "--provider-budget-usd", "--model", "--max-usd-per-request":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") || values[arg] != "" {
				return true, fmt.Errorf("%s requires one value", arg)
			}
			i++
			values[arg] = args[i]
		default:
			return true, fmt.Errorf("unknown browser %s argument %q", verb, arg)
		}
	}
	if switches["--enable"] && switches["--disable"] {
		return true, fmt.Errorf("choose --enable or --disable")
	}
	if verb != "settings" && (switches["--enable"] || switches["--disable"]) {
		return true, fmt.Errorf("enable and disable are settings options")
	}
	if verb != "credential-set" && verb != "credential-rotate" && switches["--confirm"] {
		return true, fmt.Errorf("--confirm is a credential-set option")
	}
	if verb != "provider-pricing" && (values["--model"] != "" || values["--max-usd-per-request"] != "") {
		return true, fmt.Errorf("model and per-request ceiling are provider-pricing options")
	}
	if verb != "goal" && (values["--actor"] != "" || values["--url"] != "" || values["--goal"] != "" || values["--expect-text"] != "" || values["--max-steps"] != "" || values["--provider-budget-usd"] != "") {
		return true, fmt.Errorf("task options require goal")
	}
	if verb == "route" && values["--work"] != "" {
		return true, fmt.Errorf("route is read-only")
	}
	if verb == "settings" && values["--profile"] != "" {
		return true, fmt.Errorf("profile is a route option")
	}
	if verb != "route" && verb != "goal" && values["--profile"] != "" {
		return true, fmt.Errorf("profile is a route or goal option")
	}
	homePath, err := home.Resolve(values["--home"])
	if err != nil {
		return true, err
	}
	if verb == "provider-pricing" {
		if values["--work"] == "" || values["--model"] == "" || values["--max-usd-per-request"] == "" {
			return true, fmt.Errorf("provider-pricing requires --work, --model, and --max-usd-per-request")
		}
		ceiling, parseErr := strconv.ParseFloat(values["--max-usd-per-request"], 64)
		if parseErr != nil {
			return true, fmt.Errorf("invalid provider ceiling")
		}
		cfg, _, loadErr := policy.LoadConfig(homePath)
		if loadErr != nil {
			return true, loadErr
		}
		change, setErr := SetProviderPricing(homePath, values["--work"], values["--model"], ceiling, cfg, time.Now().UTC())
		if setErr != nil {
			return true, setErr
		}
		if switches["--json"] {
			_ = json.NewEncoder(out).Encode(change)
		} else {
			_, _ = fmt.Fprintf(out, "browser provider_model=%s max_usd_per_request=%.6f changed=%t refused=%t\n", change.Settings.ProviderModel, change.Settings.ProviderMaxUSDPerRequest, change.Changed, change.Refused)
		}
		if change.Refused {
			return true, fmt.Errorf("browser provider pricing held by policy: %s", change.Reason)
		}
		return true, nil
	}
	if verb == "credential-set" || verb == "credential-rotate" {
		if values["--work"] == "" || !switches["--confirm"] {
			return true, fmt.Errorf("%s requires --work and --confirm", verb)
		}
		body, readErr := io.ReadAll(io.LimitReader(in, 4097))
		if readErr != nil || len(body) > 4096 {
			return true, fmt.Errorf("credential input could not be read within limit")
		}
		key := []byte(strings.TrimSuffix(strings.TrimSuffix(string(body), "\n"), "\r"))
		cfg, _, loadErr := policy.LoadConfig(homePath)
		if loadErr != nil {
			return true, loadErr
		}
		var change CredentialChange
		var setErr error
		if verb == "credential-rotate" {
			change, setErr = RotateTypeSafeCredential(homePath, values["--work"], key, cfg, time.Now().UTC())
		} else {
			change, setErr = StoreTypeSafeCredential(homePath, values["--work"], key, cfg, time.Now().UTC())
		}
		if setErr != nil {
			return true, setErr
		}
		if switches["--json"] {
			_ = json.NewEncoder(out).Encode(change)
		} else {
			_, _ = fmt.Fprintf(out, "browser credential_stored=%t refused=%t receipt=%s\n", change.Stored, change.Refused, change.ReceiptID)
		}
		if change.Refused {
			return true, fmt.Errorf("browser credential enrollment held by policy: %s", change.Reason)
		}
		return true, nil
	}
	if verb == "goal" {
		if values["--work"] == "" || values["--actor"] == "" || values["--url"] == "" || values["--goal"] == "" || values["--expect-text"] == "" {
			return true, fmt.Errorf("goal requires --work, --actor, --url, --goal, and --expect-text")
		}
		steps := 3
		if values["--max-steps"] != "" {
			steps, err = strconv.Atoi(values["--max-steps"])
			if err != nil {
				return true, fmt.Errorf("invalid max steps")
			}
		}
		budget := 0.0
		if values["--provider-budget-usd"] != "" {
			budget, err = strconv.ParseFloat(values["--provider-budget-usd"], 64)
			if err != nil || !finitePositive(budget) {
				return true, fmt.Errorf("goal requires positive --provider-budget-usd when supplied")
			}
		}
		cfg, _, err := policy.LoadConfig(homePath)
		if err != nil {
			return true, err
		}
		result, err := RunTask(executewire.LaunchContext(), TaskRequest{Home: homePath, Actor: values["--actor"], WorkID: values["--work"], URL: values["--url"], Goal: values["--goal"], ExpectText: values["--expect-text"], MaxSteps: steps, MaxProviderUSD: budget, ProfileID: values["--profile"]}, cfg)
		if err != nil {
			if switches["--json"] && result.Status != "" {
				_ = json.NewEncoder(out).Encode(result)
			}
			return true, err
		}
		if switches["--json"] {
			return true, json.NewEncoder(out).Encode(result)
		}
		_, err = fmt.Fprintf(out, "browser backend=%s status=%s verified=%t steps=%d\n", result.Backend, result.Status, result.Verified, result.Steps)
		return true, err
	}
	settings, err := LoadSettings(homePath)
	if err != nil {
		return true, err
	}
	if verb == "route" {
		route := SelectRoute(settings, values["--profile"])
		if switches["--json"] {
			return true, json.NewEncoder(out).Encode(route)
		}
		_, err = fmt.Fprintf(out, "browser backend=%s reason=%s\n", route.Backend, route.Reason)
		return true, err
	}
	if !switches["--enable"] && !switches["--disable"] {
		if values["--work"] != "" {
			return true, fmt.Errorf("--work requires --enable or --disable")
		}
		if switches["--json"] {
			return true, json.NewEncoder(out).Encode(settings)
		}
		_, err = fmt.Fprintf(out, "browser jev_enabled=%t\n", settings.JevEnabled)
		return true, err
	}
	if values["--work"] == "" {
		return true, fmt.Errorf("browser setting mutation requires --work")
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		return true, err
	}
	result, err := SetJevEnabled(homePath, values["--work"], switches["--enable"], cfg, time.Now().UTC())
	if err != nil {
		return true, err
	}
	if switches["--json"] {
		if err := json.NewEncoder(out).Encode(result); err != nil {
			return true, err
		}
	} else {
		if _, err := fmt.Fprintf(out, "browser jev_enabled=%t changed=%t refused=%t receipt=%s\n", result.Settings.JevEnabled, result.Changed, result.Refused, result.ReceiptID); err != nil {
			return true, err
		}
	}
	if result.Refused {
		return true, fmt.Errorf("browser setting held by policy: %s", result.Reason)
	}
	return true, nil
}
