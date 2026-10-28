package work

import (
	"path/filepath"
	"testing"
)

// TestHostProbeDistinguishesAnUnusableHost guards the guard.
//
// hostCanRespawnPanes is what decides whether a Create failure SKIPS or FAILS
// (see createForStatusTest). A probe that always returned true would red the suite
// on host load, which is the flake it exists to remove; a probe that always
// returned false would silently disable a real assertion, which is worse — the test
// would report success by never running. So the false branch needs its own proof.
//
// Note tmux AUTO-STARTS a server on a socket, so killing the server does not make a
// host unusable — an earlier version of this test asserted exactly that and was
// wrong about tmux, not about the probe. An unwritable socket path is the honest way
// to make tmux genuinely unable to provide a pane.
func TestHostProbeDistinguishesAnUnusableHost(t *testing.T) {
	t.Run("unusable socket reports false", func(t *testing.T) {
		sock := filepath.Join(t.TempDir(), "no-such-dir", "t.sock")
		if hostCanRespawnPanes(sock) {
			t.Fatal("probe reported the host CAN respawn panes on a socket tmux cannot create")
		}
	})

	t.Run("working server reports true", func(t *testing.T) {
		sock := newPrivateTmuxServer(t)
		if !hostCanRespawnPanes(sock) {
			t.Skip("this host cannot currently respawn panes, so the true branch is unprovable here")
		}
	})
}
