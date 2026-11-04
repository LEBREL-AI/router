#!/usr/bin/env bash
# Pushes the release image exactly as scripts/reproducible-build.sh builds it: same pinned BuildKit,
# same flags, OCI media types, so the digest the registry reports is the digest two clean local
# builds produced. Run after reproducible-build.sh on the same tag and compare the printed digest.
#   scripts/push-image.sh v1.2.0
set -euo pipefail
cd "$(dirname "$0")/.."
TAG="${1:?tag}"
BUILDKIT_IMAGE="moby/buildkit:v0.27.1@sha256:1e110c71d389d6d24f67b9438e2f7b8da749a6ff407b22a1631e025c95599368"
BUILDER="lebrel-release"
docker buildx inspect "$BUILDER" >/dev/null 2>&1 || docker buildx create --name "$BUILDER" --driver docker-container --driver-opt "image=$BUILDKIT_IMAGE" --bootstrap >/dev/null
docker buildx build --builder "$BUILDER" --no-cache --provenance=false --sbom=false --platform linux/amd64 \
  --build-arg SOURCE_DATE_EPOCH=0 \
  --output "type=registry,name=ghcr.io/lebrel-ai/router:$TAG,rewrite-timestamp=true,oci-mediatypes=true" . >/dev/null
docker buildx imagetools inspect "ghcr.io/lebrel-ai/router:$TAG" | grep -E "^Digest"
