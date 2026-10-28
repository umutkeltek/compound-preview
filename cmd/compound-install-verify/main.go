// Command compound-install-verify decides whether a freshly built artifact really is this
// repo's compound binary, before the installer moves it into place.
//
// It is a SEPARATE program on purpose. Asking the artifact under test to vouch for itself is
// precisely the check a stub passes: the installer's only probe used to be "run the new binary
// and compare its version string", which a four-line shell script answers correctly. This
// program is compiled from repo source by `go run` at install time, so it is trustworthy for the
// same reason the build is — nothing on the install path can substitute it.
//
// Exit codes: 0 verified, 1 verification failed (the message names the failed property), 2 usage.
package main

import (
	"flag"
	"fmt"
	"os"

	"compound/internal/arch"
)

func main() {
	fs := flag.NewFlagSet("compound-install-verify", flag.ContinueOnError)
	binary := fs.String("binary", "", "path to the built artifact to verify")
	buildSHA := fs.String("build-sha", "", "short SHA compiled into the artifact (empty means an unversioned dev build)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *binary == "" {
		fmt.Fprintln(os.Stderr, "usage: compound-install-verify --binary PATH [--build-sha SHA]")
		os.Exit(2)
	}

	report, err := arch.VerifyInstalledArtifact(*binary, *buildSHA)
	if err != nil {
		fmt.Fprintln(os.Stderr, "install verification FAILED:", err)
		fmt.Fprintln(os.Stderr, "  the artifact was NOT installed; nothing at the install path was replaced")
		os.Exit(1)
	}
	fmt.Printf("install verification ok: %s (%s, %d bytes) is compound %s build %s\n",
		report.Binary, report.Format, report.SizeBytes, report.Version, report.BuildSHA)
}
