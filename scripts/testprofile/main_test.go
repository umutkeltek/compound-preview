package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestProfileSeparatesOverlappingDurationsAndFailures(t *testing.T) {
	input := `{"Action":"start","Package":"example"}
{"Action":"run","Package":"example","Test":"TestParent"}
{"Action":"run","Package":"example","Test":"TestParent/child"}
{"Action":"fail","Package":"example","Test":"TestParent/child","Elapsed":4}
{"Action":"fail","Package":"example","Test":"TestParent","Elapsed":5}
{"Action":"fail","Package":"example","Elapsed":6}
`
	report, err := readProfile(strings.NewReader(input))
	if err != nil || !report.Complete || len(report.Packages) != 1 || len(report.Tests) != 1 || len(report.Failures) != 2 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if report.Tests[0].Seconds != 5 || report.Packages[0].Seconds != 6 {
		t.Fatalf("parent/child/package elapsed times must not be summed: %+v", report)
	}
}

func TestProfileRetainsIncompleteAndMalformedEvidence(t *testing.T) {
	for _, suffix := range []string{"", "{broken"} {
		t.Run(suffix, func(t *testing.T) {
			report, err := readProfile(strings.NewReader(`{"Action":"start","Package":"example"}
{"Action":"run","Package":"example","Test":"TestWaiting"}
{"Action":"pause","Package":"example","Test":"TestWaiting"}
` + suffix))
			if report.Complete || len(report.IncompleteTests) != 1 || len(report.IncompletePackages) != 1 {
				t.Fatalf("partial input must remain visibly incomplete: %+v", report)
			}
			if (err != nil) != (suffix != "") {
				t.Fatalf("malformed stream err=%v", err)
			}
		})
	}
	report, err := readProfile(strings.NewReader(""))
	if err != nil || report.Complete {
		t.Fatalf("empty input cannot be a complete run: %+v %v", report, err)
	}
}

func TestProfileSortsRepeatedRunsAndReportsSkippedTests(t *testing.T) {
	input := `{"Action":"start","Package":"example"}
{"Action":"run","Package":"example","Test":"TestA"}
{"Action":"pass","Package":"example","Test":"TestA","Elapsed":1}
{"Action":"run","Package":"example","Test":"TestA"}
{"Action":"pass","Package":"example","Test":"TestA","Elapsed":3}
{"Action":"run","Package":"example","Test":"TestSkip"}
{"Action":"skip","Package":"example","Test":"TestSkip","Elapsed":0}
{"Action":"pass","Package":"example","Elapsed":4}
`
	report, err := readProfile(strings.NewReader(input))
	if err != nil || !report.Complete || report.SkippedTests != 1 || len(report.Tests) != 3 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if report.Tests[0].Seconds != 3 || report.Tests[1].Seconds != 1 {
		t.Fatalf("repeated runs must stay separate and sort by elapsed: %+v", report.Tests)
	}
}

func TestProfileCommandNeverHidesFailureOrPartialInput(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        int
	}{
		{"pass", `{"Action":"pass","Package":"example","Elapsed":1}`, 0},
		{"fail", `{"Action":"fail","Package":"example","Elapsed":1}`, 1},
		{"partial", `{"Action":"start","Package":"example"}`, 1},
		{"malformed", `{"Action":"start","Package":"example"}
bad`, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			code := run(strings.NewReader(tc.input), &out, &diagnostics)
			if code != tc.want {
				t.Fatalf("exit=%d want=%d stderr=%s", code, tc.want, diagnostics.String())
			}
			var report profile
			if err := json.Unmarshal(out.Bytes(), &report); err != nil || !report.DiagnosticOnly {
				t.Fatalf("must publish explicitly non-certifying evidence: %s err=%v", out.String(), err)
			}
		})
	}
}

func TestProfileDecoderHandlesLongOutputWithoutLosingLaterEvents(t *testing.T) {
	input := `{"Action":"output","Package":"example","Output":"` + strings.Repeat("x", 128*1024) + `"}
{"Action":"pass","Package":"example","Elapsed":1}`
	report, err := readProfile(strings.NewReader(input))
	if err != nil || !report.Complete || len(report.Packages) != 1 {
		t.Fatalf("large output must not truncate the stream: %+v %v", report, err)
	}
}
