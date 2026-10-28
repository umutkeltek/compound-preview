package harness

import (
	"fmt"
	"strings"
	"testing"
)

func TestDecodeOMPPrivateEnvelopeStrictTopLevel(t *testing.T) {
	const version = "compound.omp_native_verifier_private.v0.1"
	const base = `{"schema_version":"compound.omp_native_verifier_private.v0.1","config":{},"report":{},"sanitized":{}}`
	cases := []struct{ name, body string }{
		{"duplicate_schema", strings.TrimSuffix(base, "}") + `,"schema_version":"compound.omp_native_verifier_private.v0.1"}`},
		{"duplicate_config", strings.TrimSuffix(base, "}") + `,"config":{}}`},
		{"escaped_duplicate", strings.TrimSuffix(base, "}") + `,"\u0063onfig":{}}`},
		{"case_alias_duplicate", strings.TrimSuffix(base, "}") + `,"Config":{}}`},
		{"wrong_case_key", strings.Replace(base, "schema_version", "Schema_Version", 1)},
		{"unknown_key", strings.TrimSuffix(base, "}") + `,"canary":"opaque"}`},
		{"wrong_schema", strings.Replace(base, version, "compound.omp_native_verifier_private.v0.0", 1)},
		{"array_root", "[" + base + "]"},
		{"null_root", "null"},
		{"trailing_object", base + "{}"},
		{"trailing_scalar", base + "1"},
		{"truncated", strings.TrimSuffix(base, "}")},
		{"invalid_utf8", strings.Replace(base, `"config":{}`, "\"config\":{\"canary\":\"\xff\"}", 1)},
	}
	for _, field := range []string{"config", "report", "sanitized"} {
		cases = append(cases, struct{ name, body string }{
			"missing_" + field, strings.Replace(base, fmt.Sprintf(`,"%s":{}`, field), "", 1),
		})
		for i, value := range []string{"null", `"opaque"`, "[]", "1", "true"} {
			cases = append(cases, struct{ name, body string }{
				fmt.Sprintf("non_object_%s_%d", field, i),
				strings.Replace(base, fmt.Sprintf(`"%s":{}`, field), fmt.Sprintf(`"%s":%s`, field, value), 1),
			})
		}
	}
	cases = append(cases, struct{ name, body string }{"missing_schema", `{"config":{},"report":{},"sanitized":{}}`})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeOMPPrivateEnvelope([]byte(tc.body))
			if err == nil {
				t.Fatal("expected OMP_PRIVATE_CAPTURE_SCHEMA, got nil")
			}
			if err.Error() != "OMP_PRIVATE_CAPTURE_SCHEMA" {
				t.Fatalf("error must remain content-free, got %q", err)
			}
		})
	}
}

func TestDecodeOMPPrivateEnvelopePreservesObjectPayloads(t *testing.T) {
	const version = "compound.omp_native_verifier_private.v0.1"
	const config = `{"nested":{"a":1,"extra":true},"canary":"merhaba 東京"}`
	const report = `{"values":[1,2],"nullable":null}`
	const sanitized = `{"result":{"ok":true}}`
	bodies := []string{
		fmt.Sprintf(`{"schema_version":%q,"config":%s,"report":%s,"sanitized":%s}`, version, config, report, sanitized),
		fmt.Sprintf(` { "sanitized":%s, "report":%s, "config":%s, "schema_version":%q } `, sanitized, report, config, version) + "\n\t",
	}
	for i, body := range bodies {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			got, err := decodeOMPPrivateEnvelope([]byte(body))
			if err != nil {
				t.Fatalf("valid object envelope rejected: %v", err)
			}
			if got.SchemaVersion != version || string(got.Config) != config || string(got.Report) != report || string(got.Sanitized) != sanitized {
				t.Fatal("schema or nested object bytes changed")
			}
		})
	}
}
