#!/usr/bin/env bash
# Build and publish a release: ./release.sh v0.1.0
# Local on purpose: GitHub-hosted runners don't pick up jobs for this account's
# private repos. Asset names end in <os>-<arch>, which is what
# `gh extension install/upgrade` matches on.
set -euo pipefail
tag=${1:?usage: ./release.sh vX.Y.Z}
cd "$(dirname "$0")"
[ -z "$(git status --porcelain)" ] || { echo "commit first"; exit 1; }
[ "$(git branch --show-current)" = main ] || { echo "release from main only"; exit 1; }
git fetch -q origin main
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || { echo "main is not level with origin/main"; exit 1; }
go test ./...
rm -rf dist && mkdir dist
for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  GOOS=${t%/*} GOARCH=${t#*/} CGO_ENABLED=0 \
    go build -trimpath -ldflags='-s -w' -o "dist/gh-ccsync_${tag}_${t%/*}-${t#*/}" .
done
git tag "$tag" && git push -q origin "refs/tags/$tag"
gh release create "$tag" dist/* --title "$tag" --generate-notes
