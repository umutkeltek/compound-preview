# Source-preview reporting

This repository is a source-only developer preview, with no supported signed
binary release. The publication procedure must enable GitHub private vulnerability
reporting and verify its setting before the first source push. An independent
reporter-side submission probe has not been established by the source archive.

For sensitive reports, use this repository's **Security → Advisories → Report a
vulnerability** action when it is available. If that private action is absent or
returns an error, open only a minimal public issue asking the maintainer for a
private channel. Do not include exploit details, credentials, private home data
or sensitive logs in that issue.

Use a synthetic reproduction and the exact source/binary version. Keep raw
`COMPOUND_HOME` and demo directories private. Actor labels are provenance, not
independent authentication. The inherited [security policy](SECURITY.md) describes
the upstream release floor; this preview does not assert that all supported-release
or independent reporter-side gates are complete.
