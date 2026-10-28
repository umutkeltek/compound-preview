# Release without GitHub-hosted Actions

Each SBOM inspects the freshly built executable with the explicit release
version. It does not infer the application's version from source Git tags,
so linked worktrees, shared prior clones and later tag creation preserve the
same packaged identity.

GitHub-hosted Actions have been disabled in this repository since 2026-10-16.
The tracked `.github/workflows/proof.yml` and `.github/workflows/release.yml` are
inactive references retained for tests. The single `.woodpecker.yml` pipeline
runs routine CI and release builds on the owned agent. The maintainer's Mac
rebuilds, drills, signs, and uploads a draft; only the operator publishes it.

Routine console CI initializes a disposable `COMPOUND_HOME` outside the checkout.
It uses the freshly built
CLI and requires CLI-backed integrations instead of allowing skips. It checks
projection-door and standing-law execution markers and retains the full JSON in
owned CI logs, with a copy at `.ci/reports/ci-console.json`.

## Prepare and stage

1. Complete the release floor in [SECURITY.md](../SECURITY.md), including the
   private-reporting probe, protected main, and immutable tag policy. No command
   here establishes those external repository controls. Review the candidate and
   ensure its compiled version is the intended strict stable `MAJOR.MINOR.PATCH`.
2. Capture the exact main commit. Trigger a **manual** Woodpecker pipeline for that
   commit with the custom variable `RELEASE_VERSION` set to that version, for
   example `0.1.0`. A normal push, cron, or manual run without that variable runs
   routine proof steps only. No release step runs on a tag event.
3. Wait for `prepare-bun`, `proof-gates`, `console-gates`, and `secret-scan`, then
   all three release steps. `release-build` requires the candidate to equal the
   freshly fetched `origin/main` tip, verifies modules, exports public source,
   and builds the closed nine-file bundle twice with Go 1.27.2 and
   `cyclonedx-gomod` v1.10.0. Both checksum files and every bundle file must match.
   `release-linux-drill` uses Node 22 and installs the Linux archive into disposable
   locations before running the installed acceptance suite. `release-stage` runs
   entirely as uid 1000 and copies the bundle into `/releases/<version>/` on the
   configured bind mount.

Each release step uses this Woodpecker 3.14 condition:

```yaml
when:
  - event: manual
    evaluate: 'RELEASE_VERSION != ""'
```

Conditions in one list item are ANDed; separate items are alternatives. This form
is documented by the [official 3.14 workflow syntax source](https://github.com/woodpecker-ci/woodpecker/blob/v3.14.0/docs/versioned_docs/version-3.14/20-usage/20-workflow-syntax.md#when---conditional-execution)
and its `evaluate` section, which permits custom variables. The former versioned
website URL no longer serves that version. The current
[workflow syntax page](https://woodpecker-ci.org/docs/usage/workflow-syntax#when---conditional-execution)
also describes the same form.

The stage contains exactly the nine bundle files plus `CANDIDATE`, written last:

```text
version <version>
commit <full commit>
pipeline <pipeline number>
sha256sums_sha256 <checksum-file digest>
```

Staging refuses an existing directory or symlink. An interrupted copy can leave
an incomplete directory; it is never overwritten automatically. The operator
must inspect and remove that exact candidate by hand before retrying. A completed
candidate stays immutable. Configure `COMPOUND_RELEASE_STAGE` privately as
`user@host:/absolute/path`; the publisher requires it and has no default. It is
the staging root containing version directories, not a particular version.

## Prior generation, including the first release

The installed acceptance runner requires a closed nine-file prior bundle from a
different full commit. It does not require prior publication or a different
version number. Both Linux and Mac builds use the candidate's parent (`HEAD~1`),
export its tracked source with `git archive`, and retain that actual Git commit
and index in a local shared clone for the builder's clean-source checks. They
read the parent's compiled version using `go run ./cmd/compound version` and
build with that stable version. They never invent `0.0.0-prior`, alter source
identity, or label the candidate itself as the prior generation.

This exercises upgrade failure and recovery before a first published release.
It is not evidence of compatibility with every previously published release.
Missing parent history, a parent that cannot build, or any acceptance failure
stops the release. Fetch full history before triggering the pipeline. The prior
bundle and acceptance reports are CI workspace evidence, not release assets.

## Rebuild and create a draft on the Mac

1. After staging passes, create and push an **annotated** `v<version>` tag at the
   exact staged main tip through the repository's normal operator procedure.
   The publisher never creates or pushes tags. If main has advanced, stage a new
   candidate; ancestry alone is insufficient. The local and remote tag objects
   and peeled commits must agree.
2. On Darwin/arm64, install Go 1.27.2, Node 20+, Git, GitHub CLI, OpenSSH with
   `ssh-keygen -Y`, and the usual macOS shell tools. Authenticate `gh` for the
   origin repository. Set `COMPOUND_RELEASE_STAGE` to the privately configured
   staging location and run:

   ```sh
   scripts/release-publish-local.sh 0.1.0
   ```

   The script fetches the staged candidate, checks `CANDIDATE`, creates a clean
   detached tag worktree, and installs the pinned SBOM tool into a temporary
   directory. The third build must match the staged `SHA256SUMS` and every file.
   A compiler mismatch can change bytes; it is a refusal, never a reason to skip
   comparison. The script then performs a native Darwin clean install and the
   full installed acceptance drill against the real parent bundle.
3. The script signs only `SHA256SUMS`, using `COMPOUND_RELEASE_SIGNING_KEY` when
   set or the dedicated `~/.ssh/compound_release_ed25519` key. It verifies the
   signature against `release/allowed_signers` from the clean release tag before
   uploading anything. The expected fingerprint is
   `SHA256:iAb0Eaq9vv9CrfNRy/YbitZVbaSpwqE0wY8uf5jftRE`.
4. It rechecks main and the remote tag, then uses `gh release create --draft
   --verify-tag` to upload the nine files plus `SHA256SUMS.sig`. `CANDIDATE`, the
   prior bundle, and local reports are not uploaded. An existing release causes
   refusal; the script does not overwrite assets or resume a partial draft.
   It verifies that the release remains a draft and prints download verification
   commands. Failed local runs retain their temporary evidence for inspection.
5. Review CI results, draft metadata, asset inventory, and generated notes.
   Publish explicitly through GitHub only after accepting the release floor.
   The script never changes the draft to a published release. No release is
   implied to exist merely because these instructions are present.

## Consumer verification

Follow [Getting Started](getting-started.md) to fetch the tag's `allowed_signers`,
compare its key fingerprint with [SECURITY.md](../SECURITY.md), verify the detached
SSH signature with identity and namespace `compound-release`, and check the
signed checksums before running the installer or trusting the source manifest.
This proves signing by the key the repository publishes. It does not independently
establish who controls the key beyond the repository, nor replace repository
protection or release review. Key rotation requires a reviewed trust-anchor,
fingerprint, and publisher update.
