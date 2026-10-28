package release

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestReleaseTrustAnchor(t *testing.T) {
	body, err := os.ReadFile("../../release/allowed_signers")
	if err != nil {
		t.Fatal(err)
	}
	want := "compound-release namespaces=\"compound-release\" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAID3MoT7O9hUAiDXDUM1ZuXE4TTWTYk6yDs5ea+ouv4ci\n"
	if string(body) != want {
		t.Fatal("release trust anchor changed; review key rotation explicitly")
	}
	cmd := exec.Command("ssh-keygen", "-lf", "-")
	cmd.Stdin = strings.NewReader(strings.Join(strings.Fields(want)[2:], " ") + "\n")
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "SHA256:iAb0Eaq9vv9CrfNRy/YbitZVbaSpwqE0wY8uf5jftRE") {
		t.Fatalf("signing fingerprint mismatch: %v %s", err, output)
	}
}
