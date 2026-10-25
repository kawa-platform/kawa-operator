#!/usr/bin/env bash
# Tags v<version> on origin/main and pushes it, which starts the Release workflow.
# Usage: scripts/release.sh x.y.z[-rc.N]
set -euo pipefail

version="${1:-}"
tag="v$version"

[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]] \
  || { echo "version must be x.y.z or x.y.z-rc.N (got '$version')" >&2; exit 1; }
git diff --quiet HEAD || { echo "uncommitted changes; commit or stash them first" >&2; exit 1; }
git fetch -q origin main --tags
[[ "$(git rev-parse HEAD)" == "$(git rev-parse origin/main)" ]] \
  || { echo "HEAD is not origin/main; release.yml needs the tag on main" >&2; exit 1; }
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then echo "tag $tag already exists" >&2; exit 1; fi

git tag -a "$tag" -m "$tag"
git push -q origin "$tag"
echo "pushed $tag"
