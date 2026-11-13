#!/usr/bin/env bash
# Copyright (c) 2026 Jody Scott
# SPDX-License-Identifier: Apache-2.0
#
# Tests of the release scripts (.github/workflows/release.yml): the image tags a release gets
# (ci/release-tags.sh), whether its GitHub release is Latest (ci/release-latest.sh), its notes
# (ci/release-notes.sh) and the images ci/release-image.sh builds and pushes, against a fake docker.
# Each runs in a scratch repository with the tags it names. Run by `just gates`.
set -euo pipefail

ci="$(cd "$(dirname "$0")" && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
failed=0

check() { # name got want
  if [[ "$2" == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got [$2], want [$3]"; failed=1; fi
}

# A repository with one commit per tag, in the order given; each commit's subject is "c-<tag>".
repo() {
  local dir="$scratch/$1"; shift
  rm -rf "$dir"; mkdir -p "$dir"; cd "$dir"
  git init -q -b main
  git config user.email t@example.com; git config user.name t; git config commit.gpgsign false; git config tag.gpgsign false
  git commit -q --allow-empty -m root
  for t in "$@"; do git commit -q --allow-empty -m "c-$t"; git tag -a -m "$t" "$t"; done
}

# The line tag: only to the line's highest release, never backwards, never another line.
repo tags v0.1.0
check "first release gets its line" "$("$ci/release-tags.sh" v0.1.0)" "0.1.0 0.1"
git commit -q --allow-empty -m next; git tag v0.1.1
check "v0.1.1 moves 0.1" "$("$ci/release-tags.sh" v0.1.1)" "0.1.1 0.1"
check "v0.1.0 again does not move 0.1 back" "$("$ci/release-tags.sh" v0.1.0)" "0.1.0"
repo lines v0.1.0 v0.1.9 v0.2.0 v0.1.10
check "patch 10 is above 9" "$("$ci/release-tags.sh" v0.1.10)" "0.1.10 0.1"
check "a lower patch does not move its line back" "$("$ci/release-tags.sh" v0.1.9)" "0.1.9"
check "the newer line keeps its tag" "$("$ci/release-tags.sh" v0.2.0)" "0.2.0 0.2"
check "a pre-release gets its own tag only" "$("$ci/release-tags.sh" v0.3.0-rc.1)" "0.3.0-rc.1"
check "not a version" "$("$ci/release-tags.sh" 0.1 2>/dev/null || echo refused)" "refused"
check "a tag that is not vX.Y.Z" "$("$ci/release-tags.sh" release-1 2>/dev/null || echo refused)" "refused"

# Latest: the highest release only, never a patch on an older line nor a pre-release.
check "the highest release is Latest" "$("$ci/release-latest.sh" v0.2.0)" "--latest"
check "a patch on an older line is not Latest" "$("$ci/release-latest.sh" v0.1.10)" "--latest=false"
check "a pre-release" "$("$ci/release-latest.sh" v0.3.0-rc.1)" "--prerelease"
repo latest v0.9.0 v0.10.0
check "0.10 is above 0.9" "$("$ci/release-latest.sh" v0.10.0)" "--latest"
check "0.9 is below 0.10" "$("$ci/release-latest.sh" v0.9.0)" "--latest=false"

# Notes: the commits since the previous release by version, every commit for the first.
repo notes v0.1.0
git checkout -q -b release/0.1; git commit -q --allow-empty -m c-v0.1.1; git tag v0.1.1
git checkout -q main; git commit -q --allow-empty -m c-v0.2.0; git tag v0.2.0
notes() { "$ci/release-notes.sh" "$1" | sed -E 's/ \([0-9a-f]+\)$//' | tr '\n' '|'; }
check "the first release lists every commit" "$(notes v0.1.0)" "Changes:||- c-v0.1.0|- root|"
check "a patch on an older line lists that line's commits" "$(notes v0.1.1)" "Changes since v0.1.0:||- c-v0.1.1|"
check "a release follows the previous release by version" "$(notes v0.2.0)" "Changes since v0.1.1:||- c-v0.2.0|"
check "not a version" "$("$ci/release-notes.sh" 0.2 2>/dev/null || echo refused)" "refused"

# The image: one image per platform from the Dockerfile and the binaries beside it, then one index under
# every tag. A fake docker records what it is asked.
mkdir -p "$scratch/bin" "$scratch/src" "$scratch/out/amd64" "$scratch/out/arm64" "$scratch/launcher"
cat > "$scratch/bin/docker" <<'EOF'
#!/usr/bin/env bash
echo "docker $*" >> "$DOCKER_LOG"
if [[ "$1 $2" == "buildx build" ]]; then
  while (($#)); do
    [[ "$1" == --metadata-file ]] && printf '{"containerimage.digest":"sha256:%s"}' "$ARCH_SEEN" > "$2"
    [[ "$1" == --platform ]] && ARCH_SEEN="${2#linux/}"
    shift
  done
fi
EOF
chmod +x "$scratch/bin/docker"
echo "FROM scratch" > "$scratch/src/Dockerfile"
for a in amd64 arm64; do echo "coop-$a" > "$scratch/out/$a/coop"; echo "launcher-$a" > "$scratch/launcher/launcher-linux-$a"; done
image() { # PLATFORMS args...
  local platforms="$1"; shift
  : > "$scratch/docker.log"
  PATH="$scratch/bin:$PATH" DOCKER_LOG="$scratch/docker.log" IMAGE=ghcr.io/t/coop PLATFORMS="$platforms" \
    CONTEXT_DIR="$scratch/ctx" "$ci/release-image.sh" "$scratch/src" "$scratch/out" "$scratch/launcher" "$@" > /dev/null
}
image arm64 0.1.1 abc123 0.1.1 0.1
check "arm64 only by default: one build" "$(grep -c 'buildx build' "$scratch/docker.log")" "1"
check "it is built for linux/arm64, labelled" \
  "$(grep -o -- '--platform linux/arm64 --build-arg VERSION=0.1.1 --build-arg VCS_REF=abc123' "$scratch/docker.log")" \
  "--platform linux/arm64 --build-arg VERSION=0.1.1 --build-arg VCS_REF=abc123"
check "its context holds the arm64 binaries" "$(cat "$scratch/ctx/arm64/bin/coop" "$scratch/ctx/arm64/launcher-linux-arm64")" "coop-arm64
launcher-arm64"
check "the index is tagged with each tag" "$(grep 'imagetools create' "$scratch/docker.log")" \
  "docker buildx imagetools create -t ghcr.io/t/coop:0.1.1 -t ghcr.io/t/coop:0.1 ghcr.io/t/coop@sha256:arm64"
image "arm64 amd64" 0.1.1 abc123 0.1.1
check "both platforms: two builds" "$(grep -c 'buildx build' "$scratch/docker.log")" "2"
check "both platforms: one index over both" "$(grep 'imagetools create' "$scratch/docker.log")" \
  "docker buildx imagetools create -t ghcr.io/t/coop:0.1.1 ghcr.io/t/coop@sha256:arm64 ghcr.io/t/coop@sha256:amd64"
image arm64 0.1.1 abc123
check "no tags: the dry run pushes nothing" "$(grep -c 'push=true\|imagetools' "$scratch/docker.log" || true)" "0"
check "an unknown platform is refused" "$(image "arm64 s390x" 0.1.1 abc123 2>/dev/null && echo built || echo refused)" "refused"

exit "$failed"
