package home

import "testing"

func TestParseCLIArgsTreatsFormatValueAsFlagValue(t *testing.T) {
	for _, args := range [][]string{
		{"--format", "restore", "backup", "--target", "archive.tar.gz"},
		{"backup", "--format", "restore", "--target", "archive.tar.gz"},
	} {
		parsed := ParseCLIArgs(args)
		if parsed.Subcommand != "backup" || len(parsed.RemainingPositionals) != 0 {
			t.Fatalf("ParseCLIArgs(%q) = %+v", args, parsed)
		}
	}
}
