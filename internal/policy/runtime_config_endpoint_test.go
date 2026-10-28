package policy

import (
	"encoding/json"
	"net"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

func TestRuntimeConfigInstallPrivateModelEndpoint(t *testing.T) {
	now := time.Now().UTC()
	modelURL := func(address [4]byte, port string) string {
		return (&url.URL{Scheme: "http", Host: net.JoinHostPort(netip.AddrFrom4(address).String(), port)}).String()
	}
	privateURL := modelURL([4]byte{100, 64, 0, 1}, "11434")
	credentialURL, _ := url.Parse(privateURL)
	credentialURL.User = url.UserPassword("synthetic_user", "synthetic_password")
	for _, tc := range []struct {
		name     string
		endpoint any
		valid    bool
	}{
		{"absent", nil, true},
		{"private", map[string]string{"key": "FIXTURE_OLLAMA_URL", "url": privateURL}, true},
		{"public", map[string]string{"key": "FIXTURE_OLLAMA_URL", "url": modelURL([4]byte{203, 0, 113, 1}, "11434")}, false},
		{"lan", map[string]string{"key": "FIXTURE_OLLAMA_URL", "url": modelURL([4]byte{192, 168, 1, 2}, "11434")}, false},
		{"wrong_key", map[string]string{"key": "DATABASE_URL", "url": privateURL}, false},
		{"credential", map[string]string{"key": "FIXTURE_OLLAMA_URL", "url": credentialURL.String()}, false},
		{"path", map[string]string{"key": "FIXTURE_OLLAMA_URL", "url": privateURL + "/"}, false},
		{"query", map[string]string{"key": "FIXTURE_OLLAMA_URL", "url": privateURL + "?x=1"}, false},
		{"port", map[string]string{"key": "FIXTURE_OLLAMA_URL", "url": modelURL([4]byte{100, 64, 0, 1}, "5432")}, false},
		{"extra", map[string]string{"key": "FIXTURE_OLLAMA_URL", "url": privateURL, "password": "synthetic"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := runtimeConfigInstallFixture(now)
			raw, _ := json.Marshal(r)
			var object map[string]any
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			object["profile"].(map[string]any)["api_endpoint"] = tc.endpoint
			raw, _ = json.Marshal(object)
			decoded, err := DecodeRuntimeConfigInstallRequest(raw)
			if err == nil {
				err = ValidateRuntimeConfigInstallRequest(decoded, decoded.ProgramSHA256, now)
			}
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}

func TestRuntimeConfigInstallModelEndpointCannotReplaceRoleOrPosture(t *testing.T) {
	now := time.Now().UTC()
	for _, collision := range []string{"role", "posture", "dealer"} {
		r, _ := runtimeConfigInstallFixture(now)
		r.Profile.APIEndpoint = &RuntimeConfigInstallModelEndpoint{Key: "FIXTURE_OLLAMA_URL", URL: (&url.URL{Scheme: "http", Host: net.JoinHostPort(netip.AddrFrom4([4]byte{100, 64, 0, 1}).String(), "11434")}).String()}
		switch collision {
		case "role":
			r.Roles[0].Key = r.Profile.APIEndpoint.Key
		case "posture":
			r.Profile.ProductionKey = r.Profile.APIEndpoint.Key
		case "dealer":
			r.Profile.WebDealerSource = r.Profile.APIEndpoint.Key
		}
		if err := ValidateRuntimeConfigInstallRequest(r, r.ProgramSHA256, now); err == nil {
			t.Fatalf("endpoint replaced %s", collision)
		}
	}
}
