package policy

import (
	"net/netip"
	"net/url"
	"strings"
)

// RuntimeConfigInstallModelEndpoint is a reviewed private model route, never a
// general environment override. The frozen profile and exact approval bind it.
type RuntimeConfigInstallModelEndpoint struct {
	Key string `json:"key"`
	URL string `json:"url"`
}

func ValidRuntimeConfigInstallModelEndpoint(p RuntimeConfigInstallProfile) bool {
	e := p.APIEndpoint
	if e == nil {
		return true
	}
	if !runtimeConfigInstallKey.MatchString(e.Key) || !strings.HasSuffix(e.Key, "_OLLAMA_URL") || strings.Contains(e.Key, "DATABASE") {
		return false
	}
	for _, key := range []string{p.AppKey, p.APIKey, p.MigratorKey, p.ProductionKey, p.LiveFetchKey, p.CheckoutKey, p.WebAPIKey, p.WebDealerKey, p.WebCookieKey, p.WebDealerSource} {
		if e.Key == key {
			return false
		}
	}
	u, err := url.Parse(e.URL)
	if err != nil {
		return false
	}
	address, err := netip.ParseAddr(u.Hostname())
	if err != nil || !address.Is4() || !netip.MustParsePrefix("100.64.0.0/10").Contains(address) {
		return false
	}
	return e.URL == "http://"+address.String()+":11434"
}
