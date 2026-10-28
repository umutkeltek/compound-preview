package release

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	code := m.Run()
	if err := installerFixtureDistributionTemplate.close(); err != nil {
		fmt.Fprintf(os.Stderr, "remove installer fixture distribution template: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
