package capgraph

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// A CredentialHandle says WHERE a credential lives and HOW to hand it to a child
// process. It never says WHAT the credential is.
//
// The motivating problem is credential sprawl: an operator machine accumulates
// several independent secret stores (a cloud CLI's config, a gateway's vault, a
// tool's own auth file, dotenvs, and this home's secrets/ root), and nothing can
// answer "which one holds the credential for X". Adding a store to fix that
// makes it worse. So Compound holds pointers into the stores that already exist
// and brokers access through their owners: the custodian keeps custody, rotation
// stays invisible here, and there is nothing in these records worth stealing.
//
// The rule is enforced by ValidateCredentialHandle rather than by discipline,
// because a rule that lives only in a comment is a rule that erodes.
//
// Vendor neutrality is deliberate. Kind is a MECHANISM (how injection works);
// the custodian is DATA supplied by the caller. A vendor name compiled into this
// file would be the same violation the transport enum already carries -- source
// that must change to learn about a new provider.
type CredentialHandle struct {
	// Ref addresses the credential: "<kind>://<locator>". Never a value.
	Ref string `json:"credential_ref,omitempty"`
	// Custodian names the system that holds and rotates the value, so
	// "who owns this secret" is answerable without reading the locator.
	Custodian string `json:"credential_custodian,omitempty"`
	// EnvName is the environment variable the CHILD process will see. For
	// selector-kind handles this is the selector variable itself; for brokered
	// and file kinds it is where the value lands.
	EnvName string `json:"credential_env_name,omitempty"`
	// Broker overrides the kind's default injection argv. Empty means "use the
	// built-in mechanism for this kind".
	Broker string `json:"broker_cmd,omitempty"`
}

// CredentialKind is an injection MECHANISM, not a vendor.
type CredentialKind struct {
	Name string
	// ValueTransits reports whether the secret's value passes through a process
	// Compound started. False is the strongest guarantee available: Compound
	// sets a selector and the consuming tool reads the credential itself.
	ValueTransits bool
	// LocatorMustNameVariable marks kinds whose locator addresses a container of
	// many secrets, so the variable has to be named explicitly.
	LocatorMustNameVariable bool
	Detail                  string
}

// credentialKinds is a closed set. Closed is deliberate: an unrecognized kind
// means nobody has decided how the value reaches the child, and guessing is how
// a credential ends up copied somewhere it was never meant to live.
var credentialKinds = map[string]CredentialKind{
	"selector": {
		Name: "selector", ValueTransits: false,
		Detail: "broker sets a selector variable (e.g. a named profile) and the consuming tool reads the credential itself; no value passes through Compound",
	},
	"brokered": {
		Name: "brokered", ValueTransits: true,
		Detail: "the custodian's own command injects the value into the child process environment; Compound never reads it",
	},
	"env-file": {
		Name: "env-file", ValueTransits: true, LocatorMustNameVariable: true,
		Detail: "a named variable inside an operator-managed dotenv; locator is <path>#<VAR>",
	},
	"home-secret": {
		Name: "home-secret", ValueTransits: true,
		Detail: "COMPOUND_HOME/secrets/<slug>/<file> -- ONLY for credentials no other system custodies",
	},
}

// CredentialKinds returns the known injection mechanisms keyed by name (copy;
// callers must not mutate the registry).
func CredentialKinds() map[string]CredentialKind {
	out := make(map[string]CredentialKind, len(credentialKinds))
	for k, v := range credentialKinds {
		out[k] = v
	}
	return out
}

// secretShapedPattern matches things that are credential VALUES rather than
// addresses. This is a tripwire, not a classifier: it exists so the obvious
// mistake -- pasting the key where the pointer goes -- fails loudly at the door
// instead of landing a secret in an append-only log that also feeds orient.
//
// It is deliberately biased toward false positives. Refusing a legitimate
// pointer that happens to look like a key costs one rename; accepting a real key
// writes it into a record nobody thinks of as secret-bearing, and append-only
// means it cannot be unwritten.
var secretShapedPattern = regexp.MustCompile(
	`(?i)(AKIA[0-9A-Z]{12,}|ASIA[0-9A-Z]{12,}|sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|xox[baprs]-[A-Za-z0-9-]{10,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,})`,
)

// longOpaqueRun catches the generic "someone pasted a 40-character blob" case no
// vendor prefix covers. Real locators are paths, profile names and variable
// names: they contain separators. A long unbroken run of key-alphabet characters
// is what a secret looks like and what an address does not.
var longOpaqueRun = regexp.MustCompile(`[A-Za-z0-9+/=]{40,}`)

// ValidateCredentialHandle enforces pointer-not-value at the only door that
// writes these records. An empty handle is valid: most surfaces need no
// credential at all.
func ValidateCredentialHandle(h CredentialHandle) error {
	ref := strings.TrimSpace(h.Ref)
	custodian := strings.TrimSpace(h.Custodian)
	env := strings.TrimSpace(h.EnvName)
	broker := strings.TrimSpace(h.Broker)

	if ref == "" {
		// Any other field without a ref is an instruction to invent a custodian.
		if custodian != "" || env != "" || broker != "" {
			return fmt.Errorf("credential handle needs a credential_ref before a custodian, env name, or broker")
		}
		return nil
	}

	kindName, locator, found := strings.Cut(ref, "://")
	if !found || strings.TrimSpace(kindName) == "" || strings.TrimSpace(locator) == "" {
		return fmt.Errorf("credential_ref %q must be <kind>://<locator>", ref)
	}
	kind, ok := credentialKinds[kindName]
	if !ok {
		return fmt.Errorf("credential_ref %q uses unknown kind %q; known: %s",
			ref, kindName, strings.Join(sortedKindNames(), ", "))
	}
	if custodian == "" {
		return fmt.Errorf("credential_ref %q needs a credential_custodian so %q is answerable without reading the locator",
			ref, "who holds and rotates this")
	}

	// The tripwire runs over every field, not just the ref: an operator pasting
	// a key is as likely to put it in the env-name slot.
	for label, field := range map[string]string{
		"credential_ref": ref, "credential_custodian": custodian,
		"credential_env_name": env, "broker_cmd": broker,
	} {
		if secretShapedPattern.MatchString(field) {
			return fmt.Errorf("%s looks like a credential VALUE, not an address; this store holds pointers only", label)
		}
	}
	// The opaque-run check exempts broker argv, which may legitimately carry a
	// long encoded flag, and is not persisted as an address.
	//
	// It also exempts a locator that LOOKS like a filesystem path (contains a
	// separator) for the path-shaped kinds (env-file, home-secret): a real path
	// on macOS/Windows legitimately combines alphanumerics with '/' into a 40+
	// run (/var/folders/..., the operator home). Rejecting those rejects every
	// real dotenv and custody path. A path-shaped locator with NO separator
	// (home-secret://QWERTYuiop…) is exactly the pasted blob the rule exists to
	// catch, so it stays refused. The vendor-prefixed secretShapedPattern above
	// is the tripwire that catches a pasted key in any kind.
	if pathShapedKind(kindName) && !strings.Contains(locator, "/") && !strings.Contains(locator, string(filepath.Separator)) {
		for label, field := range map[string]string{
			"credential_ref": ref, "credential_custodian": custodian, "credential_env_name": env,
		} {
			if longOpaqueRun.MatchString(field) {
				return fmt.Errorf("%s contains a 40+ character opaque run, which is the shape of a secret rather than a locator", label)
			}
		}
	}

	if env == "" {
		return fmt.Errorf("kind %q needs a credential_env_name so the broker knows what to set in the child", kindName)
	}
	if kind.LocatorMustNameVariable && !strings.Contains(locator, "#") {
		return fmt.Errorf("kind %q addresses a container of many secrets; locator %q must name the variable as <path>#<VAR>", kindName, locator)
	}
	return nil
}

func sortedKindNames() []string {
	names := make([]string, 0, len(credentialKinds))
	for name := range credentialKinds {
		names = append(names, name)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

// pathShapedKind reports whether a kind's locator is a filesystem path (so the
// opaque-run tripwire does not apply). Paths legitimately contain 40+ alnum+slash
// runs on real machines; the vendor-prefixed tripwire still catches pasted keys.
func pathShapedKind(kind string) bool {
	switch kind {
	case "env-file", "home-secret":
		return true
	default:
		return false
	}
}

// CredentialValueTransits reports whether using this handle causes the secret's
// value to pass through a process Compound started. Operators auditing exposure
// need this answerable per surface, and it is a property of the kind, so it
// cannot drift from how injection actually works.
func CredentialValueTransits(h CredentialHandle) (transits bool, known bool) {
	kindName, _, found := strings.Cut(strings.TrimSpace(h.Ref), "://")
	if !found {
		return false, false
	}
	kind, ok := credentialKinds[kindName]
	if !ok {
		return false, false
	}
	return kind.ValueTransits, true
}
