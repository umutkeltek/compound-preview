# Shared Bitwarden session adapter

This macOS adapter makes the official `bw` personal-vault CLI reusable across
authorized harnesses and fresh sessions. Compound owns this source, installer,
regression suite and installed build provenance. Bitwarden owns the encrypted
vault; macOS Keychain holds the unlock session. Browser-extension login and the
`bws` Secrets Manager CLI are separate.

Install into an initialized Compound home with the official `bw` CLI and Swift
command-line tools available on PATH:

```sh
python3 adapters/bitwarden-session/install.py --desktop
"${COMPOUND_HOME:-$HOME/.compound}/actuator/bitwarden-session" status
"${COMPOUND_HOME:-$HOME/.compound}/actuator/bitwarden-session" connect
"${COMPOUND_HOME:-$HOME/.compound}/actuator/bitwarden-session" run -- bw sync
```

`--home` selects an existing home; `--desktop` is optional. It installs a native
binary and a content-hashed build record beneath `actuator/`. Reinstalling the
same source/binary is a no-op. The installer takes the same lock as vault calls,
refuses an active operation, builds before atomic replacement, and never removes
credentials or changes the normal `bw` home. It refuses foreign Desktop launchers.

Register the installed absolute `status` command with the instance's existing
tool-topology importer; all harnesses should receive that same command and the
account-identity policy reference. Registration is discovery, not task authority.
Credential use still requires the enclosing WorkEnvelope, ActionIntent and
PolicyDecision. This adapter is session plumbing, not a new permission owner.

`connect` is a local foreground Terminal step. It prompts through `bw`, captures
the session privately, verifies it, and stores only that session in Keychain.
The master password is never stored by this adapter. The child temporarily owns
the terminal process group so interactive reads cannot stop with `SIGTTIN`.
Foreground ownership is restored on exit. A fresh process can reuse the session
through `run`; no session environment variable needs exporting between harnesses.

`status`/`doctor` returns JSON without credentials or interactive prompts. Exit 0
means unlocked; exit 3 names missing authentication, a busy operation or Keychain
access requiring local attention. Generic local failures use exit 1. `run` passes
the child's output and exit status through: consume secrets inside the child or
select metadata before emitting anything to agent transcripts. Never dump vault
items, echo tokens, or put credentials in arguments. A CLI update may require
macOS to approve Keychain access again; the session entry is preserved.

`lock` locks the CLI vault and removes this adapter's exact Keychain item. Do not
run separate `bw unlock` calls: they invalidate sessions. Per-harness vault homes
are refused. `COMPOUND_BITWARDEN_EXECUTABLE` may select an absolute official CLI
path when PATH discovery is unsuitable; keep it consistent across harnesses.

Run `go test ./adapters/bitwarden-session -count=1` on macOS. Tests use synthetic
sessions and a fake CLI, including a real controlling pseudo-terminal. They do
not log into a real account or touch the user's Keychain or vault.
