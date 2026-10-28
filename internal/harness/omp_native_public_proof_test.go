package harness

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Synthetic opaque IDs and a fixed valid RunAttempt ID; no live runtime records.
const (
	ompNativePublicProofTestWorkID         = "work_omp_native"
	ompNativePublicProofTestActionIntentID = "act_fixture"
	ompNativePublicProofTestRunAttemptID   = "ratt_20260101T000000Z_00000001"

	// opaque digest sentinels (one hex char repeated 64 times)
	ompNativePublicProofHexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ompNativePublicProofHexB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	ompNativePublicProofHexC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	ompNativePublicProofHexD = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	ompNativePublicProofHexE = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	ompNativePublicProofHexF = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	ompNativePublicProofHex0 = "0000000000000000000000000000000000000000000000000000000000000000"
	ompNativePublicProofHex7 = "7777777777777777777777777777777777777777777777777777777777777777"
)

// pre-computed substrings of the valid body so each rejection case can
// mutate exactly one structural piece without rebuilding the whole JSON.
var (
	ompNativePublicProofSchemaVersionFragment = `"schema_version":"` + ompNativePublicProofSchemaVersion + `"`
	ompNativePublicProofWorkIDFragment        = `"work_id":"` + ompNativePublicProofTestWorkID + `"`
	ompNativePublicProofRunAttemptIDFragment  = `"run_attempt_id":"` + ompNativePublicProofTestRunAttemptID + `"`
	ompNativePublicProofActionIntentFragment  = `"action_intent_id":"` + ompNativePublicProofTestActionIntentID + `"`
	ompNativePublicProofReportFragment        = `"report_sha256":"` + ompNativePublicProofHexB + `"`
	ompNativePublicProofCaptureFragment       = `"capture":"` + ompNativePublicProofHexC + `"`
	ompNativePublicProofInterpreterFragment   = `"interpreter":"` + ompNativePublicProofHexD + `"`
	ompNativePublicProofWrapperFragment       = `"wrapper":"` + ompNativePublicProofHexE + `"`
	ompNativePublicProofModelsFragment        = `"models_config":"` + ompNativePublicProofHexF + `"`
	ompNativePublicProofExecutableFragment    = `"executable":"` + ompNativePublicProofHex0 + `"`
	ompNativePublicProofRecipeRefFragment     = `"artifact:sha256:` + ompNativePublicProofHexA + `"`
	ompNativePublicProofReportRefFragment     = `"artifact:sha256:` + ompNativePublicProofHexB + `"`
	ompNativePublicProofEvidenceRefsFragment  = `"evidence_refs":[` + ompNativePublicProofRecipeRefFragment + `,` + ompNativePublicProofReportRefFragment + `]`
	ompNativePublicProofRecipeRefValue        = `artifact:sha256:` + ompNativePublicProofHexA
	ompNativePublicProofReportRefValue        = `artifact:sha256:` + ompNativePublicProofHexB
)

func validOMPNativePublicProofBody() string {
	return `{"schema_version":"` + ompNativePublicProofSchemaVersion +
		`","work_id":"` + ompNativePublicProofTestWorkID +
		`","run_attempt_id":"` + ompNativePublicProofTestRunAttemptID +
		`","action_intent_id":"` + ompNativePublicProofTestActionIntentID +
		`","report_sha256":"` + ompNativePublicProofHexB +
		`","private_custody_sha256":{` +
		ompNativePublicProofCaptureFragment + `,` +
		ompNativePublicProofInterpreterFragment + `,` +
		ompNativePublicProofWrapperFragment + `,` +
		ompNativePublicProofModelsFragment + `,` +
		ompNativePublicProofExecutableFragment +
		`},"evidence_refs":[` +
		ompNativePublicProofRecipeRefFragment + `,` +
		ompNativePublicProofReportRefFragment + `]}`
}

func assertOMPNativePublicProofZero(t *testing.T, got OMPNativePublicProof) {
	t.Helper()
	if got.SchemaVersion != "" {
		t.Fatalf("zero SchemaVersion violated: %q", got.SchemaVersion)
	}
	if got.WorkID != "" {
		t.Fatalf("zero WorkID violated: %q", got.WorkID)
	}
	if got.RunAttemptID != "" {
		t.Fatalf("zero RunAttemptID violated: %q", got.RunAttemptID)
	}
	if got.ActionIntentID != "" {
		t.Fatalf("zero ActionIntentID violated: %q", got.ActionIntentID)
	}
	if got.ReportSHA256 != "" {
		t.Fatalf("zero ReportSHA256 violated: %q", got.ReportSHA256)
	}
	if got.PrivateCustodySHA256 != nil {
		t.Fatalf("zero PrivateCustodySHA256 violated: %v", got.PrivateCustodySHA256)
	}
	if got.EvidenceRefs != nil {
		t.Fatalf("zero EvidenceRefs violated: %v", got.EvidenceRefs)
	}
}

func TestDecodeOMPNativePublicProofAcceptsExpectedShape(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "synthetic_emitted_shape",
			body: validOMPNativePublicProofBody(),
		},
		{
			name: "key_order_independent",
			body: `{"evidence_refs":[` +
				ompNativePublicProofRecipeRefFragment + `,` + ompNativePublicProofReportRefFragment + `],` +
				`"action_intent_id":"` + ompNativePublicProofTestActionIntentID + `",` +
				`"work_id":"` + ompNativePublicProofTestWorkID + `",` +
				`"private_custody_sha256":{` +
				ompNativePublicProofCaptureFragment + `,` +
				ompNativePublicProofInterpreterFragment + `,` +
				ompNativePublicProofWrapperFragment + `,` +
				ompNativePublicProofModelsFragment + `,` +
				ompNativePublicProofExecutableFragment + `},` +
				`"report_sha256":"` + ompNativePublicProofHexB + `",` +
				`"run_attempt_id":"` + ompNativePublicProofTestRunAttemptID + `",` +
				`"schema_version":"` + ompNativePublicProofSchemaVersion + `"}`,
		},
		{
			name: "single_escaped_aliases",
			body: strings.NewReplacer(
				`"schema_version"`, `"schem\u0061_version"`,
				`"capture"`, `"c\u0061pture"`,
			).Replace(validOMPNativePublicProofBody()),
		},
		{
			name: "outer_whitespace_tolerated",
			body: " \n\t" + validOMPNativePublicProofBody() + " \n\t",
		},
		{
			name: "unicode_opaque_ids",
			body: strings.NewReplacer(
				ompNativePublicProofWorkIDFragment, `"work_id":"work_örps_東京"`,
				ompNativePublicProofActionIntentFragment, `"action_intent_id":"act_ümlaut_üñîçødé"`,
			).Replace(validOMPNativePublicProofBody()),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeOMPNativePublicProof([]byte(tc.body))
			if err != nil {
				t.Fatalf("expected nil error, got %v", err)
			}
			want := OMPNativePublicProof{
				SchemaVersion:  ompNativePublicProofSchemaVersion,
				WorkID:         ompNativePublicProofTestWorkID,
				RunAttemptID:   ompNativePublicProofTestRunAttemptID,
				ActionIntentID: ompNativePublicProofTestActionIntentID,
				ReportSHA256:   ompNativePublicProofHexB,
				PrivateCustodySHA256: map[string]string{
					"capture":       ompNativePublicProofHexC,
					"interpreter":   ompNativePublicProofHexD,
					"wrapper":       ompNativePublicProofHexE,
					"models_config": ompNativePublicProofHexF,
					"executable":    ompNativePublicProofHex0,
				},
				EvidenceRefs: []string{ompNativePublicProofRecipeRefValue, ompNativePublicProofReportRefValue},
			}
			if tc.name == "unicode_opaque_ids" {
				want.WorkID = "work_örps_東京"
				want.ActionIntentID = "act_ümlaut_üñîçødé"
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("decoded proof differs: got %+v; want %+v", got, want)
			}
		})
	}
}

func TestDecodeOMPNativePublicProofRejectsBlankOpaqueIDs(t *testing.T) {
	for _, original := range []string{"work_omp_native", "act_fixture"} {
		t.Run(original, func(t *testing.T) {
			body := strings.Replace(validOMPNativePublicProofBody(), original, "   ", 1)
			if body == validOMPNativePublicProofBody() {
				t.Fatal("fixture ID replacement did not engage")
			}
			got, err := DecodeOMPNativePublicProof([]byte(body))
			if err == nil || err.Error() != "OMP_PUBLIC_PROOF_SCHEMA" {
				t.Fatalf("blank opaque ID must fail content-free: %v", err)
			}
			if !reflect.DeepEqual(got, OMPNativePublicProof{}) {
				t.Fatal("error returned a partial proof")
			}
		})
	}
}

func TestDecodeOMPNativePublicProofRejects(t *testing.T) {
	base := validOMPNativePublicProofBody()
	uppercaseHexB := strings.ToUpper(ompNativePublicProofHexB)
	uppercaseRunAttempt := strings.ToUpper(ompNativePublicProofTestRunAttemptID)
	uppercaseHexC := strings.ToUpper(ompNativePublicProofHexC)
	unicodeBad := "\xff"

	// build a malformed body where the schema_version value is replaced
	// with a raw invalid-UTF-8 byte (still inside a JSON string literal).
	invalidUTF8Body := strings.Replace(base, ompNativePublicProofSchemaVersionFragment, `"schema_version":"`+unicodeBad+`"`, 1)

	cases := []struct {
		name string
		body string
	}{
		// root shape
		{"null_root", "null"},
		{"scalar_root", "1"},
		{"array_root", "[]"},
		{"string_root", `"opaque"`},
		{"empty_body", ""},
		{"truncated_object", strings.TrimSuffix(base, "}")},
		{"truncated_inside_string", base[:strings.Index(base, ompNativePublicProofTestRunAttemptID)+8]},

		// utf-8 (raw \xff bytes injected inside quoted JSON strings)
		{"invalid_utf8_schema_version", invalidUTF8Body},
		{"invalid_utf8_work_id", strings.Replace(base, ompNativePublicProofWorkIDFragment, `"work_id":"`+unicodeBad+`"`, 1)},
		{"invalid_utf8_capture", strings.Replace(base, ompNativePublicProofCaptureFragment, `"capture":"`+unicodeBad+`"`, 1)},

		// top-level keys
		{"duplicate_schema_version", strings.TrimSuffix(base, "}") + "," + ompNativePublicProofSchemaVersionFragment + "}"},
		{"escaped_duplicate_schema_version", strings.TrimSuffix(base, "}") + `,"schem\u0061_version":"` + ompNativePublicProofSchemaVersion + `"}`},
		{"escaped_duplicate_work_id", strings.TrimSuffix(base, "}") + `,"w\u006frk_id":"work_omp_native_x"}`},
		{"case_alias_schema_version", strings.Replace(base, `"schema_version"`, `"Schema_Version"`, 1)},
		{"case_alias_work_id", strings.Replace(base, `"work_id"`, `"Work_ID"`, 1)},
		{"case_alias_capture", strings.Replace(base, `"capture"`, `"Capture"`, 1)},
		{"unknown_top_level_canary", strings.TrimSuffix(base, "}") + `,"canary":"opaque"}`},
		{"unknown_top_level_null", strings.TrimSuffix(base, "}") + `,"canary":null}`},
		{"missing_schema_version", strings.Replace(base, ompNativePublicProofSchemaVersionFragment+",", "", 1)},
		{"missing_work_id", strings.Replace(base, ompNativePublicProofWorkIDFragment+",", "", 1)},
		{"missing_run_attempt_id", strings.Replace(base, ompNativePublicProofRunAttemptIDFragment+",", "", 1)},
		{"missing_action_intent_id", strings.Replace(base, ompNativePublicProofActionIntentFragment+",", "", 1)},
		{"missing_report_sha256", strings.Replace(base, ompNativePublicProofReportFragment+",", "", 1)},
		{"missing_private_custody", strings.Replace(base, `"private_custody_sha256":{`+
			ompNativePublicProofCaptureFragment+","+
			ompNativePublicProofInterpreterFragment+","+
			ompNativePublicProofWrapperFragment+","+
			ompNativePublicProofModelsFragment+","+
			ompNativePublicProofExecutableFragment+
			`},`, "", 1)},
		{"missing_evidence_refs", strings.Replace(base, ","+ompNativePublicProofEvidenceRefsFragment, "", 1)},
		{"empty_evidence_refs", strings.Replace(base, ompNativePublicProofEvidenceRefsFragment, `"evidence_refs":[]`, 1)},

		// schema_version
		{"wrong_schema_version", strings.Replace(base, ompNativePublicProofSchemaVersion, "compound.omp_prompt_only_native_proof.v0.0", 1)},
		{"empty_schema_version", strings.Replace(base, ompNativePublicProofSchemaVersion, "", 1)},
		{"non_string_schema_version", strings.Replace(base, ompNativePublicProofSchemaVersionFragment, `"schema_version":1`, 1)},
		{"null_schema_version", strings.Replace(base, ompNativePublicProofSchemaVersionFragment, `"schema_version":null`, 1)},
		{"array_schema_version", strings.Replace(base, ompNativePublicProofSchemaVersionFragment, `"schema_version":[]`, 1)},

		// opaque IDs
		{"empty_work_id", strings.Replace(base, ompNativePublicProofWorkIDFragment, `"work_id":""`, 1)},
		{"null_work_id", strings.Replace(base, ompNativePublicProofWorkIDFragment, `"work_id":null`, 1)},
		{"non_string_work_id", strings.Replace(base, ompNativePublicProofWorkIDFragment, `"work_id":1`, 1)},
		{"non_string_run_attempt_id", strings.Replace(base, ompNativePublicProofRunAttemptIDFragment, `"run_attempt_id":1`, 1)},
		{"null_run_attempt_id", strings.Replace(base, ompNativePublicProofRunAttemptIDFragment, `"run_attempt_id":null`, 1)},
		{"empty_run_attempt_id", strings.Replace(base, ompNativePublicProofTestRunAttemptID, "", 1)},
		{"invalid_run_attempt_id", strings.Replace(base, ompNativePublicProofTestRunAttemptID, "not_a_valid_run_attempt_id", 1)},
		{"uppercase_run_attempt_id", strings.Replace(base, ompNativePublicProofTestRunAttemptID, uppercaseRunAttempt, 1)},
		{"empty_action_intent_id", strings.Replace(base, ompNativePublicProofActionIntentFragment, `"action_intent_id":""`, 1)},
		{"null_action_intent_id", strings.Replace(base, ompNativePublicProofActionIntentFragment, `"action_intent_id":null`, 1)},
		{"non_string_action_intent_id", strings.Replace(base, ompNativePublicProofActionIntentFragment, `"action_intent_id":1`, 1)},

		// report_sha256
		{"uppercase_report_sha256", strings.Replace(base, ompNativePublicProofHexB, uppercaseHexB, 1)},
		{"short_report_sha256", strings.Replace(base, ompNativePublicProofHexB, "abc", 1)},
		{"non_hex_report_sha256", strings.Replace(base, ompNativePublicProofHexB, strings.Repeat("z", ompNativePublicProofDigestHexLen), 1)},
		{"empty_report_sha256", strings.Replace(base, ompNativePublicProofReportFragment, `"report_sha256":""`, 1)},
		{"null_report_sha256", strings.Replace(base, ompNativePublicProofReportFragment, `"report_sha256":null`, 1)},
		{"non_string_report_sha256", strings.Replace(base, ompNativePublicProofReportFragment, `"report_sha256":1`, 1)},

		// private_custody_sha256 — wrong shape
		{"null_private_custody", strings.Replace(base, `"private_custody_sha256":{`+
			ompNativePublicProofCaptureFragment+","+
			ompNativePublicProofInterpreterFragment+","+
			ompNativePublicProofWrapperFragment+","+
			ompNativePublicProofModelsFragment+","+
			ompNativePublicProofExecutableFragment+
			`}`, `"private_custody_sha256":null`, 1)},
		{"array_private_custody", strings.Replace(base, `"private_custody_sha256":{`+
			ompNativePublicProofCaptureFragment+","+
			ompNativePublicProofInterpreterFragment+","+
			ompNativePublicProofWrapperFragment+","+
			ompNativePublicProofModelsFragment+","+
			ompNativePublicProofExecutableFragment+
			`}`, `"private_custody_sha256":[]`, 1)},
		{"string_private_custody", strings.Replace(base, `"private_custody_sha256":{`+
			ompNativePublicProofCaptureFragment+","+
			ompNativePublicProofInterpreterFragment+","+
			ompNativePublicProofWrapperFragment+","+
			ompNativePublicProofModelsFragment+","+
			ompNativePublicProofExecutableFragment+
			`}`, `"private_custody_sha256":"opaque"`, 1)},

		// custody roles — cardinality and shape
		{"missing_capture", strings.Replace(base, ompNativePublicProofCaptureFragment+",", "", 1)},
		{"missing_interpreter", strings.Replace(base, ompNativePublicProofInterpreterFragment+",", "", 1)},
		{"missing_wrapper", strings.Replace(base, ompNativePublicProofWrapperFragment+",", "", 1)},
		{"missing_models_config", strings.Replace(base, ompNativePublicProofModelsFragment+",", "", 1)},
		{"missing_executable", strings.Replace(base, ","+ompNativePublicProofExecutableFragment, "", 1)},
		{"extra_custody_role", strings.Replace(base, ompNativePublicProofCaptureFragment+",",
			ompNativePublicProofCaptureFragment+`,"canary":"`+ompNativePublicProofHex7+`",`, 1)},
		{"duplicate_capture_role", strings.Replace(base, ompNativePublicProofCaptureFragment+",",
			ompNativePublicProofCaptureFragment+`,"capture":"`+strings.Repeat("9", ompNativePublicProofDigestHexLen)+`",`, 1)},
		{"escaped_duplicate_capture", strings.Replace(base, ompNativePublicProofCaptureFragment+",",
			ompNativePublicProofCaptureFragment+`,"c\u0061pture":"`+strings.Repeat("9", ompNativePublicProofDigestHexLen)+`",`, 1)},
		{"uppercase_capture_digest", strings.Replace(base, ompNativePublicProofHexC, uppercaseHexC, 1)},
		{"short_capture_digest", strings.Replace(base, ompNativePublicProofHexC, "abc", 1)},
		{"non_hex_capture_digest", strings.Replace(base, ompNativePublicProofHexC, strings.Repeat("z", ompNativePublicProofDigestHexLen), 1)},
		{"null_capture_digest", strings.Replace(base, ompNativePublicProofCaptureFragment, `"capture":null`, 1)},
		{"non_string_capture_digest", strings.Replace(base, ompNativePublicProofCaptureFragment, `"capture":1`, 1)},

		// evidence_refs — cardinality and shape
		{"non_array_evidence_refs", strings.Replace(base, ompNativePublicProofEvidenceRefsFragment, `"evidence_refs":"opaque"`, 1)},
		{"null_evidence_refs", strings.Replace(base, ompNativePublicProofEvidenceRefsFragment, `"evidence_refs":null`, 1)},
		{"object_evidence_refs", strings.Replace(base, ompNativePublicProofEvidenceRefsFragment, `"evidence_refs":{}`, 1)},
		{"single_evidence_ref", strings.Replace(base, ompNativePublicProofEvidenceRefsFragment, `"evidence_refs":[`+ompNativePublicProofRecipeRefFragment+`]`, 1)},
		{"triple_evidence_refs", strings.Replace(base, ompNativePublicProofEvidenceRefsFragment,
			`"evidence_refs":[`+ompNativePublicProofRecipeRefFragment+`,`+ompNativePublicProofReportRefFragment+`,`+ompNativePublicProofRecipeRefFragment+`]`, 1)},
		{"malformed_first_evidence_ref", strings.Replace(base, ompNativePublicProofEvidenceRefsFragment,
			`"evidence_refs":["opaque",`+ompNativePublicProofReportRefFragment+`]`, 1)},
		{"non_string_first_evidence_ref", strings.Replace(base, ompNativePublicProofEvidenceRefsFragment,
			`"evidence_refs":[1,`+ompNativePublicProofReportRefFragment+`]`, 1)},

		// evidence_refs — receiver order (recipe first, report second)
		{"wrong_receiver_order_first_slot_is_report", strings.Replace(base, ompNativePublicProofEvidenceRefsFragment,
			`"evidence_refs":[`+ompNativePublicProofReportRefFragment+`,`+ompNativePublicProofRecipeRefFragment+`]`, 1)},
		{"mismatched_second_ref_digest", strings.Replace(base, ompNativePublicProofEvidenceRefsFragment,
			`"evidence_refs":[`+ompNativePublicProofRecipeRefFragment+`,"artifact:sha256:`+ompNativePublicProofHex7+`"]`, 1)},
		{"wrong_prefix_second_ref", strings.Replace(base, ompNativePublicProofEvidenceRefsFragment,
			`"evidence_refs":[`+ompNativePublicProofRecipeRefFragment+`,"sha256:`+ompNativePublicProofHexB+`"]`, 1)},
		{"uppercase_second_ref_digest", strings.Replace(base, ompNativePublicProofEvidenceRefsFragment,
			`"evidence_refs":[`+ompNativePublicProofRecipeRefFragment+`,"artifact:sha256:`+uppercaseHexB+`"]`, 1)},

		// trailing
		{"trailing_object", base + "{}"},
		{"trailing_scalar", base + "1"},
		{"trailing_array", base + "[]"},
		{"trailing_string", base + `"opaque"`},
		{"trailing_null", base + "null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertOMPNativePublicProofRejectFixture(t, tc.name, tc.body, base)
			got, err := DecodeOMPNativePublicProof([]byte(tc.body))
			if err == nil {
				t.Fatalf("expected OMP_PUBLIC_PROOF_SCHEMA, got nil; proof=%+v", got)
			}
			if err.Error() != "OMP_PUBLIC_PROOF_SCHEMA" {
				t.Fatalf("error must remain content-free, got %q", err)
			}
			assertOMPNativePublicProofZero(t, got)
		})
	}
}

// Assert fixture intent independently of the decoder under test. A negative
// case must not silently pass because its replacement did nothing or because
// invalid JSON masked the semantic rule its name claims to exercise.
func assertOMPNativePublicProofRejectFixture(t *testing.T, name, body, base string) {
	t.Helper()
	if body == base {
		t.Fatal("rejection fixture did not change the valid body")
	}
	malformed := name == "empty_body" || name == "truncated_object" ||
		name == "truncated_inside_string" || strings.HasPrefix(name, "trailing_")
	// encoding/json.Valid does not reject invalid UTF-8 inside a string.
	if !strings.HasPrefix(name, "invalid_utf8_") && json.Valid([]byte(body)) == malformed {
		t.Fatalf("fixture JSON validity does not match %s", name)
	}
	if strings.HasPrefix(name, "escaped_duplicate_") && !strings.Contains(body, `\u`) {
		t.Fatal("escaped-duplicate fixture contains no JSON escape")
	}
	if !strings.HasPrefix(name, "missing_") {
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatalf("missing-field fixture must be valid JSON: %v", err)
	}
	key := strings.TrimPrefix(name, "missing_")
	if key == "private_custody" {
		key = "private_custody_sha256"
	}
	switch key {
	case "capture", "interpreter", "wrapper", "models_config", "executable":
		var custody map[string]json.RawMessage
		if err := json.Unmarshal(fields["private_custody_sha256"], &custody); err != nil {
			t.Fatal(err)
		}
		fields = custody
	}
	if _, present := fields[key]; present {
		t.Fatalf("missing-field fixture still contains %s", key)
	}
}
