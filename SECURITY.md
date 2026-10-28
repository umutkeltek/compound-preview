# Security policy

Compound's public alpha is a local, file-backed tool for one trusted operating
system user. The supported trust boundary is defined in
[`docs/security/supported-scope.md`](docs/security/supported-scope.md). Shared
homes, remote or proxied console access, server deployment, multi-tenancy, and
Postgres-backed durable state are outside that boundary.

## Private reporting is a pre-release gate

The intended private reporting route is GitHub private vulnerability reporting
through this repository's **Security → Advisories → Report a vulnerability**
surface.

This route has not yet been verified from an authenticated reporter account.
No public-alpha tag is supported until the maintainer enables private
vulnerability reporting and records a successful reporter-side probe of that
surface. A missing report action or a 404 is not a private disclosure channel.

Until that gate is closed, do not put exploit details, credentials, private
home contents, or sensitive logs in a public issue. Open only a minimal public
issue asking the maintainer to establish a private channel, with no
vulnerability detail. The absence of a verified private route is a release
blocker, not permission to disclose sensitive material publicly.

Please include:

- the affected tagged version and `compound version --json` output;
- the supported or unsupported deployment shape in which it occurred;
- a minimal reproduction using synthetic data;
- expected and observed behavior, impact, and any known workaround;
- whether the issue is already being exploited or publicly discussed.

After the private route is verified, the maintainer will acknowledge the
report, reproduce it, agree on disclosure timing with the reporter when
practical, and publish remediation and upgrade instructions with the fix.
There is no paid bug-bounty program or response-time SLA during the public
alpha.

## Supported versions

Security fixes are made for the latest tagged public-alpha release only. No tag has been published yet, so until the first one there is no supported version. The
`main` branch and historical prerelease artifacts are development surfaces,
not supported distributions. Install only the immutable tag artifacts listed
in the release's verified `RELEASE` manifest, and verify the matching
`SHA256SUMS` before execution.

## Security-relevant data

Treat `COMPOUND_HOME` as sensitive. It can contain task context, tool output,
receipts, memory evidence, repository paths, and operational history. Keep it
on a local disk owned by the intended user, do not place it in a shared or
network directory, and do not attach it wholesale to an issue. Use synthetic
fixtures or the smallest redacted excerpt needed to reproduce a report.

## Release floor

A release candidate is not supported until its repository proof battery,
clean-room install, backup/verify/restore drill, and supported-scope live flow
have passed for the exact tagged commit. The recovery procedure is documented
in [`docs/operations/recovery.md`](docs/operations/recovery.md).

## Release signing

The dedicated release SSH key fingerprint is
`SHA256:iAb0Eaq9vv9CrfNRy/YbitZVbaSpwqE0wY8uf5jftRE`.
The trust anchor is [`release/allowed_signers`](release/allowed_signers); rotation
requires replacing it in a reviewed commit and updating the fingerprint and
publisher together. The private key remains on the maintainer's machine.

Before executing release bytes, compare the tag-bound key fingerprint above,
verify `SHA256SUMS.sig` with `ssh-keygen -Y verify` using identity and namespace
`compound-release`, then check the signed `SHA256SUMS` against the downloads.
This proves signing by the key the repository publishes; it does not independently
establish who controls that key beyond the repository. See
[Getting Started](docs/getting-started.md) for exact commands.
