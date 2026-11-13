#!/usr/bin/env bash
# Copyright (c) 2026 Jody Scott
# SPDX-License-Identifier: Apache-2.0
#
# Builds the coop binary of a release for each platform in PLATFORMS (default arm64) into OUT/<arch>/coop:
# static, cross-compiled, as the fleet's image pipeline builds it. Run in the checkout, after `just gates`
# generated tacplus/, in the build image (ci/build-image.sh; .github/workflows/release.yml).
#
# Usage:  ci/release-binaries.sh OUT
set -euo pipefail

out="$1"
for arch in ${PLATFORMS:-arm64}; do
  mkdir -p "$out/$arch"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags="-s -w" -o "$out/$arch/coop" ./cmd/coop
  echo "linux/$arch: $out/$arch/coop"
done
