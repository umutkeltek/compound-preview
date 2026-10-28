#!/usr/bin/env bash
# Rebuild, drill, sign, and upload an immutable candidate as a draft only.
set -euo pipefail

fail() { printf 'release-publish: %s\n' "$*" >&2; exit 1; }
[[ $# == 1 ]] || fail 'usage: scripts/release-publish-local.sh <version>'
version=$1
version_re='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
[[ "$version" =~ $version_re ]] || fail 'version must be strict stable SemVer'
: "${COMPOUND_RELEASE_STAGE:?set COMPOUND_RELEASE_STAGE to user@host:/absolute/path}"
[[ "$COMPOUND_RELEASE_STAGE" =~ ^[a-zA-Z0-9._-]+@[a-zA-Z0-9.-]+:/[a-zA-Z0-9/._-]+$ ]] || fail 'invalid COMPOUND_RELEASE_STAGE; require user@host:/absolute/path without shell metacharacters'
[[ "$(uname -s)/$(uname -m)" == Darwin/arm64 ]] || fail 'run natively on Darwin/arm64'
for tool in git scp go make node ssh-keygen gh shasum; do
  command -v "$tool" >/dev/null || fail "missing required tool: $tool"
done
node -e 'if (Number(process.versions.node.split(".")[0]) < 20) process.exit(1)' || fail 'Node 20+ required'
repo=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo"
tag="v$version"
git fetch origin main --tags
git fetch --no-tags origin '+refs/heads/main:refs/remotes/origin/main'
[[ "$(git cat-file -t "refs/tags/$tag")" == tag ]] || fail 'release tag must exist and be annotated'
commit=$(git rev-parse "refs/tags/$tag^{commit}")
[[ "$commit" == "$(git rev-parse refs/remotes/origin/main)" ]] || fail 'tag must equal the exact origin/main tip'
tag_object=$(git rev-parse "refs/tags/$tag")
check_remote_tag() {
  local remote
  remote=$(git ls-remote --tags origin "refs/tags/$tag" "refs/tags/$tag^{}")
  [[ "$remote" == "$(printf '%s\trefs/tags/%s\n%s\trefs/tags/%s^{}' "$tag_object" "$tag" "$commit" "$tag")" ]] || fail 'remote annotated tag differs from the verified local tag'
}
check_remote_tag
repository=$(gh repo view "$(git remote get-url origin)" --json nameWithOwner --jq .nameWithOwner)
[[ "$repository" =~ ^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$ ]] || fail 'cannot resolve origin repository identity'

root=$(mktemp -d)
source="$root/source"
cleanup() {
  local status=$?
  trap - EXIT
  if [[ -e "$source/.git" ]]; then git -C "$repo" worktree remove --force "$source" || status=1; fi
  if [[ $status == 0 ]]; then rm -rf "$root"; else printf 'release-publish: retained evidence at %s\n' "$root" >&2; fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
scp -r "$COMPOUND_RELEASE_STAGE/$version/" "$root/staged"
staged="$root/staged"
names=(RELEASE SHA256SUMS install-release.sh)
for platform in darwin_arm64 linux_amd64; do
  names+=("compound_${version}_${platform}.tar.gz" "compound_${version}_${platform}.cdx.json" "compound_${version}_${platform}_compound-breadth-engine.cdx.json")
done
[[ "$(find "$staged" -mindepth 1 -maxdepth 1 -print | wc -l)" -eq 10 ]] || fail 'staging must contain exactly nine bundle files and CANDIDATE'
for name in "${names[@]}" CANDIDATE; do
  [[ -f "$staged/$name" && ! -L "$staged/$name" ]] || fail "not a regular candidate file: $name"
done
pipeline=$(sed -n 's/^pipeline //p' "$staged/CANDIDATE")
[[ "$pipeline" =~ ^[1-9][0-9]*$ ]] || fail 'CANDIDATE lacks a pipeline number'
sums_sha=$(shasum -a 256 "$staged/SHA256SUMS" | cut -d ' ' -f 1)
printf 'version %s\ncommit %s\npipeline %s\nsha256sums_sha256 %s\n' \
  "$version" "$commit" "$pipeline" "$sums_sha" > "$root/expected-candidate"
cmp "$root/expected-candidate" "$staged/CANDIDATE" || fail 'CANDIDATE does not bind this version, tag commit, and checksum file'

git worktree add --detach "$source" "$tag"
mkdir "$root/tools"
GOBIN="$root/tools" go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.10.0
sbom_tool="$root/tools/cyclonedx-gomod"
make -C "$source" release-artifacts VERSION="$version" DIST="$root/local" SBOM_TOOL="$sbom_tool"
cmp "$root/local/SHA256SUMS" "$staged/SHA256SUMS" || fail 'Mac rebuild differs from the staged Linux build'
# Compare the closed inventory too, before executing any downloaded bytes.
for name in "${names[@]}"; do cmp "$root/local/$name" "$staged/$name"; done
(cd "$staged" && shasum -a 256 --check SHA256SUMS)

# The prior generation is the real parent commit, not an invented prerelease version.
prior_commit=$(git -C "$source" rev-parse HEAD~1)
git clone --shared --no-checkout "$source" "$root/prior-source"
git -C "$source" archive "$prior_commit" | tar -xf - -C "$root/prior-source"
git -C "$root/prior-source" reset --mixed "$prior_commit"
prior_version=$(cd "$root/prior-source" && go run ./cmd/compound version | cut -d ' ' -f 2)
VERSION="$prior_version" SOURCE_ROOT="$root/prior-source" DIST="$root/prior" SBOM_TOOL="$sbom_tool" \
  go run "$source/scripts/release-artifacts.go"
install_root="$root/clean-install"
COMPOUND_RELEASE_TARBALL="$staged/compound_${version}_darwin_arm64.tar.gz" \
  COMPOUND_HOME="$install_root/home" GOBIN="$install_root/bin" sh "$staged/install-release.sh"
[[ "$("$install_root/bin/compound" version)" == "compound $version (build $commit)" ]] || fail 'installed version or commit mismatch'
test -x "$install_root/bin/compound-breadth-engine"
# The acceptance runner requires exactly nine files, so use the identical local bundle.
node "$source/scripts/installed-release-acceptance.mjs" "$root/local" "$commit" \
  "$root/darwin-acceptance" "$root/prior" "$prior_commit"

allowed_signers="$source/release/allowed_signers"
fingerprint=$(awk '{print $3, $4}' "$allowed_signers" | ssh-keygen -lf - | awk '{print $2}')
[[ "$fingerprint" == SHA256:iAb0Eaq9vv9CrfNRy/YbitZVbaSpwqE0wY8uf5jftRE ]] || fail 'tracked release signing fingerprint changed; review the publisher and trust anchor together'
(
  cd "$staged"
  ssh-keygen -Y sign -f "${COMPOUND_RELEASE_SIGNING_KEY:-$HOME/.ssh/compound_release_ed25519}" -n compound-release SHA256SUMS
  ssh-keygen -Y verify -f "$allowed_signers" -I compound-release -n compound-release -s SHA256SUMS.sig < SHA256SUMS
)
git fetch --no-tags origin '+refs/heads/main:refs/remotes/origin/main'
[[ "$commit" == "$(git rev-parse refs/remotes/origin/main)" ]] || fail 'main moved during validation; stage a new candidate'
check_remote_tag
cat > "$root/notes.md" <<EOF
compound $version, commit $commit, Woodpecker pipeline $pipeline.

The nine-file bundle was built twice on the owned Linux agent and compared with
a third build on Darwin. Native Linux and Darwin installed acceptance precede
this draft. SHA256SUMS is SSH-signed; fingerprint: $fingerprint.
See docs/release.md and SECURITY.md at tag $tag for verification and trust limits.
The maintainer must review this draft and explicitly publish it.
EOF
files=()
for name in "${names[@]}" SHA256SUMS.sig; do files+=("$staged/$name"); done
gh release create "$tag" --repo "$repository" --draft --verify-tag \
  --target "$commit" --title "compound $version" --notes-file "$root/notes.md" "${files[@]}"
[[ "$(gh api "repos/$repository/releases/tags/$tag" --jq .draft)" == true ]] || fail 'release did not remain a draft'

cat <<EOF
Draft created for review; publication remains the operator decision.
After publication, download and verify in a fresh directory:
  VERSION=$version
  TAG=v\$VERSION
  REPOSITORY=$repository
  BASE_URL="https://github.com/\$REPOSITORY/releases/download/\$TAG"
  mkdir "compound-release-\$VERSION"
  cd "compound-release-\$VERSION"
EOF
for name in "${names[@]}" SHA256SUMS.sig; do
  printf '  curl --fail --location --output "%s" "$BASE_URL/%s"\n' "$name" "$name"
done
cat <<EOF
  curl --fail --location --output allowed_signers "https://raw.githubusercontent.com/\$REPOSITORY/\$TAG/release/allowed_signers"
  test "\$(awk '{print \$3, \$4}' allowed_signers | ssh-keygen -lf - | awk '{print \$2}')" = '$fingerprint'
  ssh-keygen -Y verify -f allowed_signers -I compound-release -n compound-release -s SHA256SUMS.sig < SHA256SUMS
  shasum -a 256 --check SHA256SUMS # Linux: sha256sum --check SHA256SUMS
The fingerprint must also match SECURITY.md. This proves signing by the key the
repository publishes; it does not independently establish who controls that key.
EOF
