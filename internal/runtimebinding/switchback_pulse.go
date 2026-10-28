package runtimebinding

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// SwitchbackPulseSchema is the canonical schema name accepted by the
	// runtime doctor. Both v1 and v2 are accepted for backward compatibility
	// while the served surface evolves; the doctor reports the observed version.
	SwitchbackPulseSchema  = "switchback/pulse@1"
	SwitchbackPulseSchema2 = "switchback/pulse@2"
	// The source collector runs on a 15-minute cadence; five minutes of bounded
	// scheduler/runtime jitter must not flap healthy evidence stale.
	SwitchbackPulseMaxAge = 20 * time.Minute

	switchbackPulseFutureSkew = time.Minute
	switchbackPulseMaxBytes   = 1 << 20
	switchbackPulseMaxChecks  = 256
	switchbackPulseMaxName    = 128
	switchbackPulseMaxDetail  = 512
)

type switchbackPulseReceipt struct {
	Schema     string                 `json:"schema"`
	Timestamp  string                 `json:"ts"`
	Mode       string                 `json:"mode"`
	Overall    string                 `json:"overall"`
	Summary    switchbackPulseSummary `json:"summary"`
	DurationMS *int64                 `json:"duration_ms"`
	Checks     []switchbackPulseCheck `json:"checks"`
}

type switchbackPulseSummary struct {
	OK   int `json:"ok"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
}

type switchbackPulseCheck struct {
	Name   string `json:"name"`
	Area   string `json:"area"`
	Level  string `json:"level"`
	Detail string `json:"detail"`
}

// LoadSwitchbackPulseReport reads an existing Switchback heartbeat receipt.
// It observes source evidence only: it never invokes Switchback or repairs runtime.
func LoadSwitchbackPulseReport(path string, now time.Time) (DoctorReport, error) {
	file, err := os.Open(path)
	if err != nil {
		return DoctorReport{}, fmt.Errorf("open switchback pulse receipt %s: %w", path, err)
	}
	defer file.Close()
	return ReadSwitchbackPulseReport(file, now)
}

// ReadSwitchbackPulseReport maps one bounded switchback/pulse@1 receipt into
// RuntimeBinding evidence. Schema and freshness failures remain DoctorReport
// checks so they can enter the existing governed writeback/repair-plan path.
func ReadSwitchbackPulseReport(reader io.Reader, now time.Time) (DoctorReport, error) {
	if reader == nil {
		return DoctorReport{}, fmt.Errorf("read switchback pulse receipt: reader required")
	}
	data, err := io.ReadAll(io.LimitReader(reader, switchbackPulseMaxBytes+1))
	if err != nil {
		return DoctorReport{}, fmt.Errorf("read switchback pulse receipt: %w", err)
	}
	if len(data) > switchbackPulseMaxBytes {
		return DoctorReport{}, fmt.Errorf("read switchback pulse receipt: exceeds %d bytes", switchbackPulseMaxBytes)
	}
	var receipt switchbackPulseReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return DoctorReport{}, fmt.Errorf("parse switchback pulse receipt: %w", err)
	}
	return switchbackPulseDoctorReport(receipt, now), nil
}

func switchbackPulseDoctorReport(receipt switchbackPulseReceipt, now time.Time) DoctorReport {
	report := DoctorReport{
		ServiceID: "service:switchback",
		Owner:     "project:switchback",
		Status:    "ok",
	}
	add := func(name, status, detail string) {
		report.Checks = append(report.Checks, Check{Name: name, Status: status, Detail: detail})
		if status == "fail" {
			report.Status = "fail"
		}
	}

	schema := strings.TrimSpace(receipt.Schema)
	if schema != SwitchbackPulseSchema && schema != SwitchbackPulseSchema2 {
		add("live:schema", "fail", "expected switchback/pulse@1 or switchback/pulse@2 receipt")
		return report
	}
	add("live:schema", "ok", schema)
	mode := strings.ToLower(strings.TrimSpace(receipt.Mode))
	if mode != "fast" && mode != "live" {
		add("live:receipt", "fail", "receipt mode must be fast or live")
		return report
	}
	if receipt.DurationMS == nil || *receipt.DurationMS < 0 {
		add("live:receipt", "fail", "receipt duration_ms must be present and non-negative")
		return report
	}
	add("live:receipt", "ok", fmt.Sprintf("mode=%s duration_ms=%d", mode, *receipt.DurationMS))

	observedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(receipt.Timestamp))
	if err != nil {
		add("live:freshness", "fail", "receipt timestamp must be RFC3339")
		return report
	}
	if now.IsZero() {
		add("live:freshness", "fail", "evaluation time is required")
		return report
	}
	if observedAt.After(now.Add(switchbackPulseFutureSkew)) {
		add("live:freshness", "fail", "receipt timestamp exceeds allowed clock skew")
		return report
	}
	age := now.Sub(observedAt)
	if age < 0 {
		age = 0
	}
	if age > SwitchbackPulseMaxAge {
		add("live:freshness", "fail", fmt.Sprintf("receipt age %s exceeds %s", age.Round(time.Second), SwitchbackPulseMaxAge))
		return report
	}
	add("live:freshness", "ok", fmt.Sprintf("receipt age %s within %s", age.Round(time.Second), SwitchbackPulseMaxAge))

	if len(receipt.Checks) == 0 {
		add("live:coverage", "fail", "pulse receipt contains no checks")
		return report
	}
	if len(receipt.Checks) > switchbackPulseMaxChecks {
		add("live:coverage", "fail", fmt.Sprintf("pulse receipt exceeds %d checks", switchbackPulseMaxChecks))
		return report
	}

	actual := switchbackPulseSummary{}
	seen := make(map[string]struct{}, len(receipt.Checks))
	coreCoverage := map[string]bool{"engine": false, "headroom": false}
	for _, sourceCheck := range receipt.Checks {
		name := sourceCheck.Name
		if !validSwitchbackPulseLabel(name) {
			add("live:coverage", "fail", "pulse receipt contains an invalid check name")
			actual.Fail++
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			add("live:coverage", "fail", "pulse receipt contains duplicate check names")
			actual.Fail++
			continue
		}
		seen[name] = struct{}{}
		if _, required := coreCoverage[name]; required {
			coreCoverage[name] = true
		}

		area := sourceCheck.Area
		if !validSwitchbackPulseLabel(area) {
			add("live:"+name, "fail", "pulse check area is invalid")
			actual.Fail++
			continue
		}

		level := strings.ToLower(strings.TrimSpace(sourceCheck.Level))
		switch level {
		case "ok":
			actual.OK++
		case "warn":
			actual.Warn++
		case "fail":
			actual.Fail++
		default:
			add("live:"+name, "fail", "area="+area+"; pulse check level is invalid")
			actual.Fail++
			continue
		}
		add("live:"+name, level, switchbackPulseDetail(area, sourceCheck.Detail))
	}
	var missingCore []string
	for _, name := range []string{"engine", "headroom"} {
		if !coreCoverage[name] {
			missingCore = append(missingCore, name)
		}
	}
	if len(missingCore) > 0 {
		add("live:coverage", "fail", "pulse receipt missing required checks: "+strings.Join(missingCore, ", "))
	} else {
		add("live:coverage", "ok", "required checks present: engine, headroom")
	}

	if receipt.Summary != actual {
		add("live:summary", "fail", fmt.Sprintf(
			"receipt summary does not match checks: got ok=%d warn=%d fail=%d; derived ok=%d warn=%d fail=%d",
			receipt.Summary.OK, receipt.Summary.Warn, receipt.Summary.Fail,
			actual.OK, actual.Warn, actual.Fail,
		))
	} else {
		add("live:summary", "ok", fmt.Sprintf("ok=%d warn=%d fail=%d", actual.OK, actual.Warn, actual.Fail))
	}

	derivedOverall := "ok"
	if actual.Fail > 0 {
		derivedOverall = "fail"
	} else if actual.Warn > 0 {
		derivedOverall = "warn"
	}
	if strings.ToLower(strings.TrimSpace(receipt.Overall)) != derivedOverall {
		add("live:overall", "fail", "receipt overall does not match check levels")
	}

	return report
}

func validSwitchbackPulseLabel(value string) bool {
	if strings.TrimSpace(value) == "" || !utf8.ValidString(value) {
		return false
	}
	count := 0
	for _, char := range value {
		count++
		if count > switchbackPulseMaxName || unicode.IsControl(char) || unicode.In(char, unicode.Cf) {
			return false
		}
	}
	return true
}

func switchbackPulseDetail(area, detail string) string {
	var safe strings.Builder
	for _, char := range detail {
		if unicode.IsControl(char) || unicode.In(char, unicode.Cf) {
			safe.WriteByte(' ')
			continue
		}
		safe.WriteRune(char)
	}
	clean := strings.Join(strings.Fields(safe.String()), " ")
	runes := []rune(clean)
	if len(runes) > switchbackPulseMaxDetail {
		clean = string(runes[:switchbackPulseMaxDetail])
	}
	if clean == "" {
		return "area=" + area
	}
	return "area=" + area + "; " + clean
}
