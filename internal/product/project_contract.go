package product

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"compound/internal/projectcontract"
)

const projectContractSchemaVersion = "1"

type projectContract struct {
	Project struct {
		Slug    string `yaml:"slug"`
		Name    string `yaml:"name"`
		Class   string `yaml:"class"`
		Status  string `yaml:"status"`
		Owner   string `yaml:"owner"`
		Notes   string `yaml:"notes"`
		Aliases []any  `yaml:"aliases"`
	} `yaml:"project"`
	Source struct {
		Authority struct {
			Host string `yaml:"host"`
			Path string `yaml:"path"`
			Repo string `yaml:"repo"`
		} `yaml:"authority"`
	} `yaml:"source"`
	Runtime struct {
		Authority    string   `yaml:"authority"`
		ServiceNames []string `yaml:"service_names"`
		RuntimePaths []string `yaml:"runtime_paths"`
		URLs         []string `yaml:"urls"`
		Healthchecks []string `yaml:"healthchecks"`
		Primary      string   `yaml:"primary"`
		Deployment   struct {
			Method   string `yaml:"method"`
			Command  string `yaml:"command"`
			Rollback string `yaml:"rollback"`
		} `yaml:"deployment"`
	} `yaml:"runtime"`
	Data struct {
		Classification  string                   `yaml:"classification"`
		LocalPaths      []string                 `yaml:"local_paths"`
		Databases       projectcontract.NameList `yaml:"databases"`
		ExternalSources []string                 `yaml:"external_sources"`
		Backend         string                   `yaml:"backend"`
		CloudResources  string                   `yaml:"cloud_resources"`
	} `yaml:"data"`
	ReleaseIdentity releaseIdentity `yaml:"release_identity"`
}

type releaseContract struct {
	Project         string          `yaml:"project"`
	ReleaseStatus   string          `yaml:"release_status"`
	ReleaseIdentity releaseIdentity `yaml:"release_identity"`
}

type releaseIdentity struct {
	BundleID             string   `yaml:"bundle_id"`
	AppStoreConnectAppID string   `yaml:"app_store_connect_app_id"`
	SubscriptionIDs      []string `yaml:"subscription_product_ids"`
	Version              string   `yaml:"version"`
	Build                any      `yaml:"build"`
}

// LoadProjectContractBundle adapts infra-os-style project/release contracts into
// the existing Product registry bundle. It is source-in-place intake only:
// analytics and causality still enter through ProductSignal.
func LoadProjectContractBundle(projectPath, releasePath string) (Bundle, error) {
	hasProject := strings.TrimSpace(projectPath) != ""
	hasRelease := strings.TrimSpace(releasePath) != ""
	if !hasProject && !hasRelease {
		return Bundle{}, fmt.Errorf("project or release contract path required")
	}

	var (
		pc         projectContract
		rc         releaseContract
		projectRef string
		releaseRef string
		err        error
	)
	if hasProject {
		pc, err = loadProjectContract(projectPath)
		if err != nil {
			return Bundle{}, err
		}
		projectRef = filepath.Base(projectPath)
	}
	if hasRelease {
		rc, err = loadReleaseContract(releasePath)
		if err != nil {
			return Bundle{}, err
		}
		releaseRef = filepath.Base(releasePath)
	}
	bundle, err := ProjectContractBundle(pc, rc, projectRef, releaseRef)
	if err != nil || !hasProject {
		return bundle, err
	}
	// The directory the contract was read from is the product's checkout on
	// this machine. source.authority.path names the authority host's path,
	// which is often a different machine, so only this one lets an agent
	// standing in the checkout ask "which product is this?".
	if dir, err := filepath.Abs(filepath.Dir(projectPath)); err == nil && !hasSurfaceRefValue(bundle.Product.Surfaces, dir) {
		bundle.Product.Surfaces = append(bundle.Product.Surfaces, Surface{ID: "checkout", Kind: "repo", Ref: dir})
	}
	return bundle, nil
}

func hasSurfaceRefValue(surfaces []Surface, ref string) bool {
	for _, s := range surfaces {
		if s.Ref == ref {
			return true
		}
	}
	return false
}

func loadProjectContract(path string) (projectContract, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return projectContract{}, fmt.Errorf("read project contract %s: %w", path, err)
	}
	var pc projectContract
	if err := decodeProjectContractYAML(data, &pc); err != nil {
		return projectContract{}, fmt.Errorf("parse project contract %s: %w", path, err)
	}
	if genericProjectIdentity(pc.Project.Slug) {
		return projectContract{}, fmt.Errorf("project contract canonical identity %q is generic", pc.Project.Slug)
	}
	if !supportedProjectIdentitySyntax(pc.Project.Slug) {
		return projectContract{}, fmt.Errorf("project contract canonical identity %q contains unsupported punctuation", pc.Project.Slug)
	}
	return pc, nil
}

func loadReleaseContract(path string) (releaseContract, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return releaseContract{}, fmt.Errorf("read release contract %s: %w", path, err)
	}
	var rc releaseContract
	if err := decodeProjectContractYAML(data, &rc); err != nil {
		return releaseContract{}, fmt.Errorf("parse release contract %s: %w", path, err)
	}
	if genericProjectIdentity(rc.Project) {
		return releaseContract{}, fmt.Errorf("release contract canonical identity %q is generic", rc.Project)
	}
	if !supportedProjectIdentitySyntax(rc.Project) {
		return releaseContract{}, fmt.Errorf("release contract canonical identity %q contains unsupported punctuation", rc.Project)
	}
	return rc, nil
}

func decodeProjectContractYAML(data []byte, out any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("contract must contain exactly one YAML document")
	}
	if len(document.Content) != 1 {
		return fmt.Errorf("contract must contain one YAML document root")
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("contract root must be a YAML mapping")
	}
	if err := validateProjectContractMappingKeys(root); err != nil {
		return err
	}
	version := projectContractMappingValue(root, "schema_version")
	if version == nil || version.Kind != yaml.ScalarNode {
		return fmt.Errorf("schema_version is required and must be a scalar")
	}
	switch version.Tag {
	case "", "!!int", "!!str":
	default:
		return fmt.Errorf("schema_version has unsupported YAML tag %q", version.Tag)
	}
	if strings.TrimSpace(version.Value) != projectContractSchemaVersion {
		return fmt.Errorf("unsupported schema_version %q", version.Value)
	}
	if err := root.Decode(out); err != nil {
		return err
	}
	return nil
}

func validateProjectContractMappingKeys(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.MappingNode {
		if len(node.Content)%2 != 0 {
			return fmt.Errorf("YAML mapping must contain key-value pairs")
		}
		seen := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode {
				return fmt.Errorf("YAML mapping key must be a scalar")
			}
			normalized := strings.ToLower(strings.TrimSpace(key.Value))
			switch {
			case normalized == "":
				return fmt.Errorf("YAML mapping key must not be empty")
			case normalized == "<<":
				return fmt.Errorf("YAML merge keys are not supported")
			case seen[normalized]:
				return fmt.Errorf("duplicate YAML mapping key %q", key.Value)
			}
			seen[normalized] = true
			if err := validateProjectContractMappingKeys(node.Content[i+1]); err != nil {
				return err
			}
		}
		return nil
	}
	for _, child := range node.Content {
		if err := validateProjectContractMappingKeys(child); err != nil {
			return err
		}
	}
	return nil
}

func projectContractMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if strings.EqualFold(strings.TrimSpace(mapping.Content[i].Value), key) {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func ProjectContractBundle(pc projectContract, rc releaseContract, projectRef, releaseRef string) (Bundle, error) {
	slug := strings.TrimSpace(pc.Project.Slug)
	if slug == "" {
		slug = strings.TrimSpace(rc.Project)
	}
	if slug == "" {
		return Bundle{}, fmt.Errorf("project contract project.slug required")
	}
	if genericProjectIdentity(slug) {
		return Bundle{}, fmt.Errorf("project contract canonical identity %q is generic", slug)
	}
	if !supportedProjectIdentitySyntax(slug) {
		return Bundle{}, fmt.Errorf("project contract canonical identity %q contains unsupported punctuation", slug)
	}
	if rc.Project != "" && pc.Project.Slug != "" && strings.TrimSpace(rc.Project) != strings.TrimSpace(pc.Project.Slug) {
		return Bundle{}, fmt.Errorf("release project %q does not match project slug %q", rc.Project, pc.Project.Slug)
	}
	name := firstNonEmpty(pc.Project.Name, slug)
	identity := mergeReleaseIdentity(pc.ReleaseIdentity, rc.ReleaseIdentity)
	productID := "product:" + normalizeIDPart(slug)
	evidenceRefs := compact([]string{optionalRef("project-contract:", projectRef), optionalRef("release-contract:", releaseRef)})

	bundle := Bundle{
		SchemaVersion: SchemaVersion,
		Product: Product{
			ID:              productID,
			Name:            name,
			Aliases:         projectAliases(pc),
			OwnerEntity:     ownerEntity(pc.Project.Owner, slug),
			Surfaces:        projectSurfaces(pc, identity, slug),
			Channels:        projectChannels(pc, identity),
			Positioning:     projectPositioning(pc, rc, name),
			PrivacyPosture:  projectPrivacy(pc),
			ReleaseIdentity: exportedReleaseIdentity(identity),
			Deployment:      projectDeployment(pc),
			// No TasteProfile, Essence, Forms, or Learned: a project contract
			// declares where a product runs, not its taste or market essence, so
			// its import never writes or supersedes taste.* or essence.* facts.
		},
		EvidenceRefs:    evidenceRefs,
		contractSourced: true,
	}
	if err := Validate(bundle); err != nil {
		return Bundle{}, err
	}
	return bundle, nil
}

func exportedReleaseIdentity(identity releaseIdentity) ReleaseIdentity {
	build := ""
	if identity.Build != nil {
		build = strings.TrimSpace(fmt.Sprint(identity.Build))
	}
	return ReleaseIdentity{
		BundleID:             strings.TrimSpace(identity.BundleID),
		AppStoreConnectAppID: strings.TrimSpace(identity.AppStoreConnectAppID),
		SubscriptionIDs:      compact(identity.SubscriptionIDs),
		Version:              strings.TrimSpace(identity.Version),
		Build:                build,
	}
}

func mergeReleaseIdentity(project, release releaseIdentity) releaseIdentity {
	out := project
	if strings.TrimSpace(release.BundleID) != "" {
		out.BundleID = release.BundleID
	}
	if strings.TrimSpace(release.AppStoreConnectAppID) != "" {
		out.AppStoreConnectAppID = release.AppStoreConnectAppID
	}
	if len(release.SubscriptionIDs) > 0 {
		out.SubscriptionIDs = append([]string(nil), release.SubscriptionIDs...)
	}
	if strings.TrimSpace(release.Version) != "" {
		out.Version = release.Version
	}
	if release.Build != nil {
		out.Build = release.Build
	}
	return out
}

// projectAliases renders the contract's project.aliases as names. Contracts
// write them as plain strings, and a name containing ": " arrives from YAML as a
// one-entry map ("Bar Exam: State Law Reference"); both are the author's alias,
// so the map is joined back into the text they typed rather than dropped.
func projectAliases(pc projectContract) []string {
	var out []string
	for _, raw := range pc.Project.Aliases {
		switch v := raw.(type) {
		case string:
			out = append(out, v)
		case map[string]any:
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				out = append(out, strings.TrimSpace(fmt.Sprintf("%s: %v", key, v[key])))
			}
		}
	}
	return compact(out)
}

func projectSurfaces(pc projectContract, id releaseIdentity, slug string) []Surface {
	var surfaces []Surface
	add := func(surfaceID, kind, ref string) {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return
		}
		surfaces = append(surfaces, Surface{ID: surfaceID, Kind: kind, Ref: ref})
	}
	add("source-repo", "repo", pc.Source.Authority.Repo)
	if pc.Source.Authority.Repo == "" {
		add("source-path", "repo", pc.Source.Authority.Path)
	}
	add("bundle", "app_bundle", optionalRef("bundle:", id.BundleID))
	add("app-store", "app_store", optionalRef("appstore:", id.AppStoreConnectAppID))
	for i, url := range pc.Runtime.URLs {
		add(fmt.Sprintf("url-%d", i+1), "web", url)
	}
	for i, url := range pc.Runtime.Healthchecks {
		add(fmt.Sprintf("healthcheck-%d", i+1), "healthcheck", url)
	}
	if len(surfaces) == 0 {
		add("project", "project", slug)
	}
	return surfaces
}

// projectDeployment derives where the product runs. The place is the runtime
// authority, falling back to the source host — the same resolution
// internal/runtimebinding already performs on this contract shape, kept
// identical so the two readers cannot disagree about one product's host. When
// neither resolves to a real name the deployment stays empty; the method and
// command are carried only alongside a place, since a command without a
// machine is an invitation to run it on the wrong one.
func projectDeployment(pc projectContract) Deployment {
	place := normalizePlaceID(pc.Runtime.Authority)
	if place == "" {
		place = normalizePlaceID(pc.Source.Authority.Host)
	}
	if place == "" {
		return Deployment{}
	}
	return Deployment{
		Place:        "place:" + place,
		Method:       concreteValue(pc.Runtime.Deployment.Method),
		Command:      concreteValue(pc.Runtime.Deployment.Command),
		Rollback:     concreteValue(pc.Runtime.Deployment.Rollback),
		Services:     compact(pc.Runtime.ServiceNames),
		Healthchecks: compact(pc.Runtime.Healthchecks),
	}
}

// concreteValue drops placeholder text real contracts carry ("unknown", "tbd",
// "none"). An absent field reads as "not known here, go ask"; a field that says
// "unknown" reads as an answer, and an agent can act on an answer.
func concreteValue(value string) string {
	if genericProjectIdentity(value) {
		return ""
	}
	return strings.TrimSpace(value)
}

// normalizePlaceID lowercases a host or platform name while preserving the
// punctuation that makes it addressable. It deliberately does not reuse
// normalizeIDPart: that collapses `.` to `_`, which would turn a dotted host
// such as `edge.example` into `edge_example` and break the join with the
// services registry (compound://registries/services). A generic value
// ("unknown", "none") yields "".
func normalizePlaceID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if genericProjectIdentity(value) {
		return ""
	}
	var b strings.Builder
	prevSeparator := false
	for _, r := range value {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevSeparator = false
		case r == '.' || r == '-' || r == '_' || r == '/':
			if !prevSeparator {
				b.WriteRune(r)
				prevSeparator = true
			}
		default:
			if !prevSeparator {
				b.WriteByte('-')
				prevSeparator = true
			}
		}
	}
	return strings.Trim(b.String(), "._-/")
}

func projectChannels(pc projectContract, id releaseIdentity) []string {
	var channels []string
	if strings.TrimSpace(id.AppStoreConnectAppID) != "" || strings.TrimSpace(id.BundleID) != "" {
		channels = append(channels, "app_store")
	}
	if len(pc.Runtime.URLs) > 0 {
		channels = append(channels, "web")
	}
	if len(pc.Runtime.Healthchecks) > 0 || len(pc.Runtime.ServiceNames) > 0 {
		channels = append(channels, "api")
	}
	if strings.Contains(strings.ToLower(pc.Runtime.Primary), "ios") {
		channels = append(channels, "ios")
	}
	if strings.TrimSpace(pc.Source.Authority.Repo) != "" || strings.TrimSpace(pc.Source.Authority.Path) != "" {
		channels = append(channels, "repo")
	}
	if len(channels) == 0 {
		channels = append(channels, "source_contract")
	}
	return compact(channels)
}

func projectPositioning(pc projectContract, rc releaseContract, name string) string {
	if strings.TrimSpace(pc.Project.Notes) != "" {
		return strings.TrimSpace(pc.Project.Notes)
	}
	if strings.TrimSpace(rc.ReleaseStatus) != "" {
		return fmt.Sprintf("%s release status: %s.", name, strings.TrimSpace(rc.ReleaseStatus))
	}
	return fmt.Sprintf("%s product source contract (%s/%s).", name, firstNonEmpty(pc.Project.Class, "unknown_class"), firstNonEmpty(pc.Project.Status, "unknown_status"))
}

func projectPrivacy(pc projectContract) string {
	if strings.TrimSpace(pc.Data.Classification) != "" {
		return strings.TrimSpace(pc.Data.Classification)
	}
	if strings.TrimSpace(pc.Data.Backend) == "none" || strings.Contains(strings.ToLower(strings.Join(pc.Data.LocalPaths, " ")), "local") {
		return "local_first"
	}
	return "unspecified_project_contract"
}

func ownerEntity(owner, slug string) string {
	owner = normalizeIDPart(owner)
	if owner != "" {
		return "owner:" + owner
	}
	return "project:" + normalizeIDPart(slug)
}

func genericProjectIdentity(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "-", "unknown", "none", "null", "n/a", "na", "tbd", "todo", "not_configured", "not-configured", "unconfigured":
		return true
	default:
		return false
	}
}

func supportedProjectIdentitySyntax(value string) bool {
	hasAlphanumeric := false
	for _, r := range strings.TrimSpace(value) {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') {
			hasAlphanumeric = true
			continue
		}
		if r == '-' ||
			r == '_' ||
			unicode.IsSpace(r) {
			continue
		}
		return false
	}
	return hasAlphanumeric
}

func optionalRef(prefix, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return prefix + value
}
