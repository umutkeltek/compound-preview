package policy

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// writeRetentionJSONImpl marshals v as JSON and writes to w. Returns 0 on
// success, 1 on a write error.
func writeRetentionJSONImpl(w io.Writer, v any) int {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return 1
	}
	if _, err := io.WriteString(w, string(data)+"\n"); err != nil {
		return 1
	}
	return 0
}

// humanBytes renders a byte count as a terse human-readable string.
// Mirrors cmd/compound/retention_cli.go's humanBytes. Kept here so the
// extracted retention_cli.go body has the same on-wire shape without
// importing a formatting helper from cmd/compound (which would cycle).
func humanBytes(n int64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
	)
	switch {
	case n >= GiB:
		return fmtFloat(float64(n)/float64(GiB)) + " GiB"
	case n >= MiB:
		return fmtFloat(float64(n)/float64(MiB)) + " MiB"
	case n >= KiB:
		return fmtFloat(float64(n)/float64(KiB)) + " KiB"
	default:
		return fmtInt(n) + " B"
	}
}

// formatString is the package's bridge to fmt.Sprintf without re-importing
// here (the policy package already imports fmt at the top of other files).
func formatString(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

// fmtFloat renders a float with up-to-two-decimal precision, stripping
// trailing zeros for a compact look (e.g. 1.00 → 1, 1.50 → 1.5).
func fmtFloat(f float64) string {
	s := formatString("%.2f", f)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	return s
}

// fmtInt renders an int64 without importing strconv (the policy package
// already uses fmt elsewhere).
func fmtInt(n int64) string {
	return formatString("%d", n)
}
