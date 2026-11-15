#!/usr/bin/env bash
# Copyright (c) 2026 Jody Scott
# SPDX-License-Identifier: Apache-2.0
#
# Assembles the release image (.github/workflows/release.yml) from the Dockerfile, once per platform, and
# publishes them as one image.
#
# Usage:  ci/release-image.sh SRC BINS LAUNCHERS VERSION REVISION [TAG...]
#
# The Dockerfile takes its binaries from beside it, as the fleet's image pipeline puts them there: for each
# platform in PLATFORMS (default arm64) this stages a build context with SRC's Dockerfile, BINS/<arch>/coop as
# bin/coop and LAUNCHERS/launcher-linux-<arch>, and builds it labelled with VERSION and REVISION (the commit).
# With TAGs, each platform's image is pushed by digest and $IMAGE:<TAG> (each TAG) is pointed at all of them.
# Without, nothing is pushed: the dry run. Tested by ci/release_test.sh.
set -euo pipefail

src="$1" bins="$2" launchers="$3" version="$4" revision="$5"
shift 5
image="${IMAGE:-ghcr.io/tacplus-dev/coop}"
contexts="${CONTEXT_DIR:-$src/.release/image}"

for arch in ${PLATFORMS:-arm64}; do
  case "$arch" in amd64|arm64) ;; *) echo "release-image.sh: unsupported platform $arch (amd64, arm64)" >&2; exit 1 ;; esac
done
digests=()
for arch in ${PLATFORMS:-arm64}; do
  ctx="$contexts/$arch"
  rm -rf "$ctx"
  mkdir -p "$ctx/bin"
  cp "$src/Dockerfile" "$ctx/"
  cp "$bins/$arch/coop" "$ctx/bin/coop"
  cp "$launchers/launcher-linux-$arch" "$ctx/"

  output="type=cacheonly"
  (($#)) && output="type=image,name=$image,push-by-digest=true,name-canonical=true,push=true"
  docker buildx build --platform "linux/$arch" \
    --build-arg "VERSION=$version" --build-arg "VCS_REF=$revision" \
    --output "$output" --metadata-file "$ctx.json" "$ctx"
  if (($#)); then
    digest="$(sed -n -E 's/.*"containerimage\.digest"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' "$ctx.json")"
    [[ -n "$digest" ]] || { echo "release-image.sh: no digest for linux/$arch" >&2; exit 1; }
    echo "linux/$arch: $image@$digest"
    digests+=("$image@$digest")
  fi
done

(($#)) || exit 0
refs=()
for tag in "$@"; do refs+=(-t "$image:$tag"); done
docker buildx imagetools create "${refs[@]}" "${digests[@]}"
