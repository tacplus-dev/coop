#!/usr/bin/env bash
# Copyright (c) 2026 Jody Scott
# SPDX-License-Identifier: Apache-2.0
#
# Runs a command as CI runs this repository's builds (.github/workflows/release.yml): in the exact tacplus
# build image the checkout's tacplus.lock pins, by digest (`tacplus pin` writes it), on the checkout. A newer
# patch arrives only as a commit that moves the lock (tacplus-dev/roadmap decisions/deterministic-builds-2026-10-27.md).
# It needs no credentials.
#
# Usage:  ci/build-image.sh SRC CMD [ARG...]
#
# SRC is the checkout (its tacplus.lock picks the image, and CMD runs there); this repository's own checkout
# is mounted too, at its own path, so CMD may be one of its scripts and may write under it. HOME is BUILD_HOME (default a fresh one under /tmp), so
# runs that share it share Go's module and build caches. PLATFORMS is passed through.
set -euo pipefail

src="$(cd "$1" && pwd)"
shift
root="$(cd "$(dirname "$0")/.." && pwd)"
lock="$src/tacplus.lock"
[[ -f "$lock" ]] || { echo "build-image.sh: no $lock: run \`tacplus pin\` in the build image of the floor tacplus.yaml declares and commit it" >&2; exit 1; }
image="$(sed -n -E 's/^build_image:[[:space:]]*"?([^"[:space:]]+)"?.*$/\1/p' "$lock" | head -1)"
[[ "$image" == *@sha256:* ]] || { echo "build-image.sh: $lock names no build_image by digest" >&2; exit 1; }

home="${BUILD_HOME:-$(mktemp -d)}"
mkdir -p "$home"
mounts=(-v "$root:$root" -v "$home:$home")
[[ "$src" == "$root" || "$src" == "$root"/* ]] || mounts+=(-v "$src:$src")
docker run --rm -u "$(id -u):$(id -g)" -e HOME="$home" -e PLATFORMS="${PLATFORMS:-arm64}" \
  "${mounts[@]}" -w "$src" "$image" "$@"
