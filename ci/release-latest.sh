#!/usr/bin/env bash
# Copyright (c) 2026 Jody Scott
# SPDX-License-Identifier: Apache-2.0
#
# The `gh release create` flag of a release (.github/workflows/release.yml): --latest for the highest release
# of all, --latest=false for any other (a patch on an older line is never Latest), --prerelease for vX.Y.Z-pre.
#
# Usage:  ci/release-latest.sh vX.Y.Z[-pre]
#
# Run in the checkout with its tags fetched. Tested by ci/release_test.sh.
set -euo pipefail

ref="$1"
if [[ "$ref" == *-* ]]; then
  echo --prerelease
elif [[ "$(git tag -l 'v*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -t. -k1.2,1n -k2,2n -k3,3n | tail -1)" == "$ref" ]]; then
  echo --latest
else
  echo --latest=false
fi
