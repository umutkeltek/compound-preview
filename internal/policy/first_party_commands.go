package policy

import (
	"fmt"
	"strings"

	homepkg "compound/internal/home"
)

type FirstPartyCommandAdmission string

const (
	FirstPartyCommandRead     FirstPartyCommandAdmission = "read"
	FirstPartyCommandGoverned FirstPartyCommandAdmission = "governed"
	FirstPartyCommandInstall  FirstPartyCommandAdmission = "install"
	FirstPartyCommandHold     FirstPartyCommandAdmission = "hold"
)

type firstPartyCommandRule struct {
	admission       FirstPartyCommandAdmission
	subcommand      map[string]FirstPartyCommandAdmission
	subcommandFlags map[string]map[string]FirstPartyCommandAdmission
}

// FirstPartyCommandSpec is executable-registry admission metadata before it is
// compiled into the catalog used at dispatch. The executable owns handlers;
// policy owns how their command, subcommand, and flag postures compose.
type FirstPartyCommandSpec struct {
	Admission               FirstPartyCommandAdmission
	SubcommandAdmission     map[string]FirstPartyCommandAdmission
	SubcommandFlagAdmission map[string]map[string]FirstPartyCommandAdmission
}

func NewFirstPartyCommandSpec(admission FirstPartyCommandAdmission) FirstPartyCommandSpec {
	return FirstPartyCommandSpec{Admission: validFirstPartyAdmission(admission)}
}

func (spec FirstPartyCommandSpec) WithSubcommand(
	subcommand string,
	admission FirstPartyCommandAdmission,
) FirstPartyCommandSpec {
	next := spec.clone()
	if next.SubcommandAdmission == nil {
		next.SubcommandAdmission = map[string]FirstPartyCommandAdmission{}
	}
	next.SubcommandAdmission[subcommand] = validFirstPartyAdmission(admission)
	return next
}

func (spec FirstPartyCommandSpec) WithSubcommandFlag(
	subcommand string,
	flag string,
	admission FirstPartyCommandAdmission,
) FirstPartyCommandSpec {
	next := spec.clone()
	if next.SubcommandFlagAdmission == nil {
		next.SubcommandFlagAdmission = map[string]map[string]FirstPartyCommandAdmission{}
	}
	if next.SubcommandFlagAdmission[subcommand] == nil {
		next.SubcommandFlagAdmission[subcommand] = map[string]FirstPartyCommandAdmission{}
	}
	next.SubcommandFlagAdmission[subcommand][flag] = validFirstPartyAdmission(admission)
	return next
}

func (spec FirstPartyCommandSpec) clone() FirstPartyCommandSpec {
	next := FirstPartyCommandSpec{
		Admission:               spec.Admission,
		SubcommandAdmission:     make(map[string]FirstPartyCommandAdmission, len(spec.SubcommandAdmission)),
		SubcommandFlagAdmission: make(map[string]map[string]FirstPartyCommandAdmission, len(spec.SubcommandFlagAdmission)),
	}
	for subcommand, admission := range spec.SubcommandAdmission {
		next.SubcommandAdmission[subcommand] = admission
	}
	for subcommand, flags := range spec.SubcommandFlagAdmission {
		next.SubcommandFlagAdmission[subcommand] = make(map[string]FirstPartyCommandAdmission, len(flags))
		for flag, admission := range flags {
			next.SubcommandFlagAdmission[subcommand][flag] = admission
		}
	}
	return next
}

// FirstPartyCommandCatalog is the shell monitor's read-only view of the
// executable Compound CLI registry. It deliberately knows command names and
// admission posture, including the small number of flags that make an otherwise
// mutating subcommand provably read-only. It does not infer vendor behavior.
//
// A zero catalog fails closed. The Compound executable supplies its runtime
// registry at the hook seam, so policy cannot silently drift from a second
// handwritten shell allowlist.
type FirstPartyCommandCatalog struct {
	commands map[string]firstPartyCommandRule
}

func NewFirstPartyCommandCatalog(commands map[string]FirstPartyCommandAdmission) FirstPartyCommandCatalog {
	catalog := FirstPartyCommandCatalog{
		commands: make(map[string]firstPartyCommandRule, len(commands)),
	}
	for name, admission := range commands {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		catalog.commands[name] = firstPartyCommandRule{admission: validFirstPartyAdmission(admission)}
	}
	return catalog
}

func NewFirstPartyCommandCatalogFromSpecs(specs map[string]FirstPartyCommandSpec) FirstPartyCommandCatalog {
	commands := make(map[string]FirstPartyCommandAdmission, len(specs))
	for name, spec := range specs {
		commands[name] = spec.Admission
	}
	catalog := NewFirstPartyCommandCatalog(commands)
	for name, spec := range specs {
		catalog = catalog.WithSubcommands(name, spec.SubcommandAdmission)
		for subcommand, flags := range spec.SubcommandFlagAdmission {
			for flag, admission := range flags {
				catalog = catalog.WithSubcommandFlagAdmission(name, subcommand, flag, admission)
			}
		}
	}
	return catalog
}

func (c FirstPartyCommandCatalog) Lookup(name string) (FirstPartyCommandAdmission, bool) {
	rule, ok := c.commands[strings.ToLower(strings.TrimSpace(name))]
	return rule.admission, ok
}

func (c FirstPartyCommandCatalog) LookupSubcommand(
	name string,
	subcommand string,
) (FirstPartyCommandAdmission, bool) {
	return c.LookupSubcommandWithFlags(name, subcommand, nil)
}

func (c FirstPartyCommandCatalog) LookupSubcommandWithFlags(
	name string,
	subcommand string,
	args []string,
) (FirstPartyCommandAdmission, bool) {
	rule, ok := c.commands[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return "", false
	}
	subcommand = strings.ToLower(strings.TrimSpace(subcommand))
	if flagAdmissions := rule.subcommandFlags[subcommand]; len(flagAdmissions) > 0 {
		for _, arg := range args {
			if admission, exists := flagAdmissions[arg]; exists {
				return admission, true
			}
		}
	}
	if admission, exists := rule.subcommand[subcommand]; exists {
		return admission, true
	}
	return rule.admission, true
}

// RequireHomeCompatibility binds the executable command registry's admission
// posture to the selected Compound home before dispatch. Read/help invocations
// remain available for recovery. Init and restore enforce compatibility at
// their own fresh-target boundaries.
func (c FirstPartyCommandCatalog) RequireHomeCompatibility(command string, args []string) error {
	command = strings.ToLower(strings.TrimSpace(command))
	subcommand := firstPartySubcommand(args)
	// Product's root parser accepts only a sole help token before any mutation.
	if command == "product" && len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		return nil
	}
	// cmdSourceObserve accepts only the exact nested sole help (observe plus
	// one help token) before resolving a home or reading any file. Bind the
	// exemption to that exact parser shape; mixed or trailing help keeps the
	// ordinary mutation admission.
	if command == "source" && len(args) == 2 && args[0] == "observe" && (args[1] == "--help" || args[1] == "-h") {
		return nil
	}
	// These root parsers now return for sole help before any home access.
	// Mixed or trailing help keeps the ordinary mutation admission.
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		switch command {
		case "finding", "knowledge", "writeback", "placement", "contract", "envelope", "schema", "runtime":
			return nil
		}
	}
	// cmdHome and cmdHookInstall check hasUsageFlag before resolving a home or
	// dispatching any mutation. Bind these exemptions to those exact parsers
	// instead of treating help as a global admission bypass for handlers that
	// may ignore it.
	if firstPartyUsageRequested(args) && (command == "home" || (command == "hook" && subcommand == "install")) {
		return nil
	}
	admission, ok := c.LookupSubcommandWithFlags(command, subcommand, args)
	if !ok {
		return fmt.Errorf("command %q has no first-party admission metadata", command)
	}
	if admission == FirstPartyCommandRead {
		return nil
	}
	if command == "home" && (subcommand == "" || subcommand == "init" || subcommand == "restore") {
		return nil
	}
	// Runtime hooks own a protocol-shaped fail-open/deny response. Letting a
	// generic pre-dispatch error replace that response makes the harness treat
	// an unavailable home as silence. The hook composition root separately
	// proves compatibility before it gives the hook any writable home path.
	if command == "hook" && firstPartyRuntimeHook(subcommand, args) {
		return nil
	}
	// proof-check --ci / --ci-console are the fresh-clone-portable gate
	// subsets: every gate needing home state is either hostBound (excluded
	// from these subsets) or self-initializes an isolated temp home
	// (proof_check.go / proof_check_memory_gate.go home.Init(tmpHome, ...)).
	// Without this exemption a bare machine — CI runner, fresh checkout,
	// downstream re-runner — cannot even start the portable battery, because
	// admission demands an existing home manifest before dispatch. The
	// work-driven canonical proof path (no portable flag) keeps the full
	// mutation admission.
	if command == "proof-check" && (firstPartyFlagPresent(args, "--ci") || firstPartyFlagPresent(args, "--ci-console")) {
		return nil
	}
	// cmdProofCheck refuses a malformed --redact-review invocation (missing or
	// flag-shaped ActionIntent ID, or a review combined with a lightweight or
	// recovery mode) with a usage error and exit 2 before it resolves any home
	// or runs a gate. Bind that exemption to the exact refusal shape so a bare
	// machine (CI runner, fresh checkout) gets the same usage refusal instead
	// of a missing-manifest error; a well-formed review keeps full admission.
	if command == "proof-check" && firstPartyProofCheckReviewRefused(args) {
		return nil
	}
	homePath, err := homepkg.Resolve("")
	if command == "home" {
		if explicitHome, ok, selectionErr := firstPartyHomeFlagValue(args); selectionErr != nil {
			return selectionErr
		} else if ok {
			homePath, err = homepkg.Resolve(explicitHome)
		} else if firstPartyFlagPresent(args, "--local") {
			homePath, err = homepkg.LocalPath()
		}
	}
	if err != nil {
		return err
	}
	return homepkg.RequireMutationCompatible(homePath)
}

func firstPartySubcommand(args []string) string {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "--home", "--archive", "--target":
			if index+1 < len(args) {
				index++
			}
			continue
		}
		arg = strings.TrimSpace(arg)
		if arg != "" && arg != "--" && !strings.HasPrefix(arg, "-") {
			return strings.ToLower(arg)
		}
	}
	return ""
}

func firstPartyHomeFlagValue(args []string) (string, bool, error) {
	selected := ""
	found := false
	for index := 0; index < len(args); index++ {
		if args[index] != "--home" {
			continue
		}
		if index+1 >= len(args) || strings.TrimSpace(args[index+1]) == "" {
			return "", false, fmt.Errorf("--home requires a non-blank value")
		}
		selected = args[index+1]
		found = true
		index++
	}
	return selected, found, nil
}

func firstPartyUsageRequested(args []string) bool {
	return firstPartyFlagPresent(args, "-h") || firstPartyFlagPresent(args, "--help")
}

func firstPartyRuntimeHook(subcommand string, args []string) bool {
	switch strings.ToLower(strings.TrimSpace(subcommand)) {
	case "session-start", "user-prompt-submit", "pre-tool-use", "post-tool-use", "session-end", "stop":
		return true
	case "session-end-adapter":
		// Live SessionEnd is a native hook callback and must fail open with its
		// protocol response. Explicit replay is an operator mutation and keeps
		// the ordinary compatibility gate.
		return firstPartyFlagPresent(args, "--live")
	default:
		return false
	}
}

// firstPartyProofCheckReviewRefused mirrors cmdProofCheck's pre-home usage
// refusals for --redact-review exactly: the flag without an ID (or with a
// flag-shaped value), or an ID combined with --dev/--presubmit/--dry-run/--ci/
// --ci-console/--recover-legacy-retained/--timings. Any other shape is not a
// refusal and must keep the ordinary home admission.
func firstPartyProofCheckReviewRefused(args []string) bool {
	if !firstPartyFlagPresent(args, "--redact-review") {
		return false
	}
	id := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--redact-review" && i+1 < len(args) {
			id = strings.TrimSpace(args[i+1])
			break
		}
	}
	if id == "" || strings.HasPrefix(id, "--") {
		return true
	}
	for _, flag := range []string{"--dry-run", "--ci", "--ci-console", "--recover-legacy-retained", "--timings"} {
		if firstPartyFlagPresent(args, flag) {
			return true
		}
	}
	for _, arg := range args {
		if arg == "--dev" || strings.HasPrefix(arg, "--dev=") || arg == "--presubmit" || strings.HasPrefix(arg, "--presubmit=") {
			return true
		}
	}
	return false
}

func firstPartyFlagPresent(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

func (c FirstPartyCommandCatalog) WithSubcommands(
	name string,
	subcommands map[string]FirstPartyCommandAdmission,
) FirstPartyCommandCatalog {
	next := c.clone()
	commandName := strings.ToLower(strings.TrimSpace(name))
	rule, ok := next.commands[commandName]
	if !ok {
		return next
	}
	rule.subcommand = make(map[string]FirstPartyCommandAdmission, len(subcommands))
	for subcommand, admission := range subcommands {
		subcommand = strings.ToLower(strings.TrimSpace(subcommand))
		if subcommand != "" {
			rule.subcommand[subcommand] = validFirstPartyAdmission(admission)
		}
	}
	next.commands[commandName] = rule
	return next
}

func (c FirstPartyCommandCatalog) WithSubcommandFlagAdmission(
	name string,
	subcommand string,
	flag string,
	admission FirstPartyCommandAdmission,
) FirstPartyCommandCatalog {
	next := c.clone()
	commandName := strings.ToLower(strings.TrimSpace(name))
	rule, ok := next.commands[commandName]
	if !ok {
		return next
	}
	subcommand = strings.ToLower(strings.TrimSpace(subcommand))
	flag = strings.TrimSpace(flag)
	if subcommand == "" || flag == "" {
		return next
	}
	if rule.subcommandFlags == nil {
		rule.subcommandFlags = map[string]map[string]FirstPartyCommandAdmission{}
	}
	if rule.subcommandFlags[subcommand] == nil {
		rule.subcommandFlags[subcommand] = map[string]FirstPartyCommandAdmission{}
	}
	rule.subcommandFlags[subcommand][flag] = validFirstPartyAdmission(admission)
	next.commands[commandName] = rule
	return next
}

func (c FirstPartyCommandCatalog) Without(name string) FirstPartyCommandCatalog {
	next := c.clone()
	without := strings.ToLower(strings.TrimSpace(name))
	delete(next.commands, without)
	return next
}

func (c FirstPartyCommandCatalog) clone() FirstPartyCommandCatalog {
	next := FirstPartyCommandCatalog{
		commands: make(map[string]firstPartyCommandRule, len(c.commands)),
	}
	for command, rule := range c.commands {
		subcommands := make(map[string]FirstPartyCommandAdmission, len(rule.subcommand))
		for subcommand, admission := range rule.subcommand {
			subcommands[subcommand] = admission
		}
		rule.subcommand = subcommands
		subcommandFlags := make(map[string]map[string]FirstPartyCommandAdmission, len(rule.subcommandFlags))
		for subcommand, admissions := range rule.subcommandFlags {
			flags := make(map[string]FirstPartyCommandAdmission, len(admissions))
			for flag, admission := range admissions {
				flags[flag] = admission
			}
			subcommandFlags[subcommand] = flags
		}
		rule.subcommandFlags = subcommandFlags
		next.commands[command] = rule
	}
	return next
}

func validFirstPartyAdmission(admission FirstPartyCommandAdmission) FirstPartyCommandAdmission {
	switch admission {
	case FirstPartyCommandRead,
		FirstPartyCommandGoverned,
		FirstPartyCommandInstall,
		FirstPartyCommandHold:
		return admission
	default:
		return FirstPartyCommandHold
	}
}
