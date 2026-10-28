package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"compound/internal/mem"
	"compound/internal/product"
)

const productRecallFenceAuthorityRef = "compound://contract/product-recall-fence"

// productRecallFence is the single product-isolation seam consumed by flat
// memory, folded positions, accepted decisions, and grouped probes. It scopes
// product-associated claims while leaving genuinely global claims available.
// An empty Entity on a required fence is deliberate fail-closed state.
type productRecallFence struct {
	Required bool
	Status   string
	Identity string
	Entity   string
	products productIdentityIndex
	Revision string
}

type productIdentityIndex struct {
	canonical map[string][]string
	exact     map[string][]string
}

func resolveProductRecallFence(in Input, view *mem.CurrentView) productRecallFence {
	if !strings.EqualFold(strings.TrimSpace(in.RepoKind), "product") {
		return productRecallFence{Status: "not-required"}
	}
	fence := productRecallFence{Required: true, Status: "unresolved"}
	if view == nil {
		fence.Revision = productRecallFenceRevision(fence)
		return fence
	}

	claims := view.Claims()
	fence.products = currentProductIdentityIndex(claims)
	canonicalProductID, contractStatus := nearestProductContractIdentity(in.Cwd)
	if contractStatus != "valid" {
		fence.Status = "contract-" + contractStatus
		fence.Revision = productRecallFenceRevision(fence)
		return fence
	}
	candidates := fence.products.canonical[canonicalProductEntityKey(canonicalProductID)]
	switch len(candidates) {
	case 1:
		fence.Status = "resolved"
		fence.Entity = candidates[0]
		fence.Identity = strings.TrimPrefix(canonicalProductEntityKey(candidates[0]), "product:")
	case 0:
		fence.Status = "unresolved"
	default:
		fence.Status = "ambiguous"
	}
	fence.Revision = productRecallFenceRevision(fence)
	return fence
}

func structuralProductEntityAlias(value string) (string, bool) {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if !strings.HasPrefix(lower, "repo:") && !strings.HasPrefix(lower, "workspace:") {
		return "", false
	}
	i := strings.LastIndex(value, ":")
	if i < 0 || i+1 >= len(value) {
		return "", false
	}
	return strings.TrimSpace(value[i+1:]), true
}

func currentProductIdentityIndex(claims []mem.Claim) productIdentityIndex {
	byCanonical := map[string]map[string]bool{}
	byExact := map[string]map[string]bool{}
	for _, claim := range claims {
		entity := strings.TrimSpace(claim.Entity)
		// Canonical product identities have one namespace separator. Deeper
		// product:* evidence namespaces (for example product:unscoped:idea)
		// are not products and cannot establish a workspace identity.
		if !strings.HasPrefix(strings.ToLower(entity), "product:") || strings.Count(entity, ":") != 1 {
			continue
		}
		canonicalKey := canonicalProductEntityKey(entity)
		exactKey := exactProductIdentity(entity)
		if canonicalKey == "" || exactKey == "" {
			continue
		}
		if byCanonical[canonicalKey] == nil {
			byCanonical[canonicalKey] = map[string]bool{}
		}
		byCanonical[canonicalKey][entity] = true
		if byExact[exactKey] == nil {
			byExact[exactKey] = map[string]bool{}
		}
		byExact[exactKey][entity] = true
	}
	return productIdentityIndex{
		canonical: sortedProductIdentityIndex(byCanonical),
		exact:     sortedProductIdentityIndex(byExact),
	}
}

func canonicalProductEntityKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func sortedProductIdentityIndex(source map[string]map[string]bool) map[string][]string {
	out := make(map[string][]string, len(source))
	for key, entities := range source {
		for entity := range entities {
			out[key] = append(out[key], entity)
		}
		sort.Strings(out[key])
	}
	return out
}

func exactProductIdentity(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if strings.HasPrefix(value, "product:") {
		value = strings.TrimSpace(strings.TrimPrefix(value, "product:"))
	}
	var b strings.Builder
	boundary := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if boundary && b.Len() > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
			boundary = false
			continue
		}
		if r == '-' || r == '_' || unicode.IsSpace(r) {
			if b.Len() > 0 {
				boundary = true
			}
			continue
		}
		// Slash, dot, and other punctuation are not boundary-preserving
		// product identity. Reject them instead of collapsing resemblance.
		return ""
	}
	return b.String()
}

func productFieldKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if i := strings.LastIndex(value, ":"); i >= 0 && i+1 < len(value) {
		value = value[i+1:]
	}
	return compactProductIdentity(value)
}

func compactProductIdentity(value string) string {
	var b strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// nearestProductContractIdentity owns only trusted workspace discovery. The
// product package owns contract parsing, validation, and canonical identity.
func nearestProductContractIdentity(cwd string) (string, string) {
	dir, err := canonicalWorkspacePath(cwd)
	if err != nil {
		return "", "invalid"
	}
	ceiling, found, err := trustedProductContractCeiling(dir)
	if err != nil {
		return "", "invalid"
	}
	if !found {
		return "", "untrusted"
	}
	for {
		projectPath, projectPresent, projectErr := resolveOptionalProductContract(filepath.Join(dir, "project.yaml"), ceiling)
		releasePath, releasePresent, releaseErr := resolveOptionalProductContract(filepath.Join(dir, "release.yaml"), ceiling)
		if projectErr != nil || releaseErr != nil {
			return "", "invalid"
		}
		if projectPresent || releasePresent {
			bundle, loadErr := product.LoadProjectContractBundle(projectPath, releasePath)
			if loadErr != nil {
				return "", "invalid"
			}
			productID := canonicalProductEntityKey(bundle.Product.ID)
			if !strings.HasPrefix(productID, "product:") || strings.Count(productID, ":") != 1 {
				return "", "invalid"
			}
			return productID, "valid"
		}
		if dir == ceiling {
			return "", "absent"
		}
		parent := filepath.Dir(dir)
		if parent == dir || !workspacePathContains(ceiling, parent) {
			return "", "invalid"
		}
		dir = parent
	}
}

func trustedProductContractCeiling(dir string) (string, bool, error) {
	for {
		for _, marker := range []string{".git", repoMarker} {
			if _, err := os.Lstat(filepath.Join(dir, marker)); err == nil {
				return dir, true, nil
			} else if !os.IsNotExist(err) {
				return "", false, err
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false, nil
		}
		dir = parent
	}
}

func resolveOptionalProductContract(path, ceiling string) (string, bool, error) {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false, err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", false, err
	}
	if !workspacePathContains(ceiling, resolved) {
		return "", false, os.ErrPermission
	}
	return resolved, true, nil
}

func canonicalWorkspacePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", os.ErrInvalid
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	physical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(physical), nil
}

func workspacePathContains(root, cwd string) bool {
	root = strings.TrimSpace(root)
	cwd = strings.TrimSpace(cwd)
	if root == "" || cwd == "" || !filepath.IsAbs(root) {
		return false
	}
	var err error
	root, err = canonicalWorkspacePath(root)
	if err != nil {
		return false
	}
	cwd, err = canonicalWorkspacePath(cwd)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(root, cwd)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func productRecallFenceAllowsClaim(fence productRecallFence, claim mem.Claim) bool {
	if !fence.Required {
		return true
	}
	associated, unknown := productEntitiesForClaim(fence, claim)
	if len(associated) == 0 && !unknown {
		return true
	}
	return !unknown && fence.Entity != "" && len(associated) == 1 && associated[fence.Entity]
}

func productEntitiesForClaim(fence productRecallFence, claim mem.Claim) (map[string]bool, bool) {
	associated := map[string]bool{}
	unknown := false
	// Association is structural, never inferred from arbitrary prose. A global
	// law or decision may cite a product as evidence without belonging to that
	// product; importers that mean product scope must emit an entity, topic ref,
	// qualifier, or product-ref predicate.
	add := func(value string, unknownWhenUnresolved bool) {
		sawToken := false
		for _, token := range strings.FieldsFunc(value, func(r rune) bool {
			return r == ',' || r == ';' || r == '|'
		}) {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}
			sawToken = true
			exact := fence.products.exact[exactProductIdentity(token)]
			if len(exact) > 0 {
				for _, entity := range exact {
					associated[entity] = true
				}
				continue
			}
			if strings.HasPrefix(strings.ToLower(token), "product:") {
				unknown = true
				continue
			}
			if unknownWhenUnresolved {
				unknown = true
			}
		}
		if unknownWhenUnresolved && !sawToken {
			unknown = true
		}
	}
	entity := strings.TrimSpace(claim.Entity)
	add(entity, strings.HasPrefix(strings.ToLower(entity), "product:"))
	if alias, ok := structuralProductEntityAlias(entity); ok {
		add(alias, false)
	}
	for _, topicRef := range claim.TopicRefs {
		add(topicRef, strings.HasPrefix(strings.ToLower(strings.TrimSpace(topicRef)), "product:"))
	}
	for key, value := range claim.Qualifiers {
		switch productFieldKey(key) {
		case "product", "productid", "productref", "scopeproduct":
			add(value, true)
		case "domain", "domains", "topic", "scope", "scopes":
			add(value, strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "product:"))
		}
	}
	switch productFieldKey(claim.Predicate) {
	case "product", "productid", "productref", "scopeproduct":
		add(claim.Object, true)
	}
	return associated, unknown
}

func seedProductFenceEntity(entities map[string]bool, fence productRecallFence) {
	if fence.Entity != "" {
		entities[memoryKey(fence.Entity)] = true
	}
}

func productRecallFenceRevision(fence productRecallFence) string {
	body := strings.Join([]string{
		productRecallFenceAuthorityRef,
		strings.TrimSpace(fence.Status),
		strings.TrimSpace(fence.Identity),
		strings.TrimSpace(fence.Entity),
	}, "\x00")
	digest := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(digest[:])
}
