#!/usr/bin/env bash
# Builds the linux/amd64 router image twice from a clean cache, with a pinned BuildKit, and compares
# the OCI manifest digests AND the compression-independent identity (image config without history,
# uncompressed layer diff_ids). Any difference is a bug: the image is meant to be rebuildable, bit for
# bit, by anyone who wants to check what runs. The working tree must be clean and on a tag.
#   scripts/reproducible-build.sh [tag]        (OUT_DIR=dist by default; SKIP_GIT_CHECK=1 for dev)
set -euo pipefail
cd "$(dirname "$0")/.."
TAG="${1:-dev}"
OUT="${OUT_DIR:-dist}"
# The builder: a fixed BuildKit, so the compression of the layers (and therefore the manifest
# digest) is the same for everyone; this is the version RELEASES.md names per row.
BUILDKIT_IMAGE="moby/buildkit:v0.27.1@sha256:1e110c71d389d6d24f67b9438e2f7b8da749a6ff407b22a1631e025c95599368"
BUILDER="lebrel-release"
if [ "${SKIP_GIT_CHECK:-}" != "1" ]; then
  if [ -n "$(git status --porcelain)" ]; then echo "working tree not clean: a release builds exactly what the tag holds" >&2; exit 1; fi
  if [ "$TAG" != "dev" ] && [ "$(git describe --tags --exact-match 2>/dev/null || true)" != "$TAG" ]; then
    echo "HEAD is not tag $TAG ($(git describe --tags --always))" >&2; exit 1
  fi
fi
if docker buildx inspect "$BUILDER" >/dev/null 2>&1; then
  if ! docker buildx inspect "$BUILDER" | grep -q "BuildKit version: *v0.27.1"; then
    echo "builder $BUILDER runs another BuildKit; recreating it with $BUILDKIT_IMAGE" >&2
    docker buildx rm "$BUILDER" >/dev/null
  fi
fi
docker buildx inspect "$BUILDER" >/dev/null 2>&1 || docker buildx create --name "$BUILDER" --driver docker-container --driver-opt "image=$BUILDKIT_IMAGE" --bootstrap >/dev/null
mkdir -p "$OUT"
build() {
  docker buildx build --builder "$BUILDER" --no-cache --provenance=false --sbom=false --platform linux/amd64 \
    --build-arg SOURCE_DATE_EPOCH=0 \
    --output "type=oci,dest=$OUT/router-$TAG-$1.tar,rewrite-timestamp=true" . >/dev/null
  python3 - "$OUT/router-$TAG-$1.tar" <<'PY'
import hashlib, json, sys, tarfile
with tarfile.open(sys.argv[1]) as tar:
    index = json.load(tar.extractfile("index.json"))
    manifest_digest = index["manifests"][0]["digest"]
    manifest = json.load(tar.extractfile("blobs/" + manifest_digest.replace(":", "/")))
    config = json.load(tar.extractfile("blobs/" + manifest["config"]["digest"].replace(":", "/")))
    identity = {"config": config.get("config"), "diff_ids": config["rootfs"]["diff_ids"], "architecture": config.get("architecture"), "os": config.get("os")}
    identity_digest = "sha256:" + hashlib.sha256(json.dumps(identity, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
print(manifest_digest, identity_digest)
PY
}
read -r A_MANIFEST A_IDENTITY <<<"$(build a)"
read -r B_MANIFEST B_IDENTITY <<<"$(build b)"
echo "build a: manifest $A_MANIFEST identity $A_IDENTITY"
echo "build b: manifest $B_MANIFEST identity $B_IDENTITY"
if [ "$A_MANIFEST" != "$B_MANIFEST" ] || [ "$A_IDENTITY" != "$B_IDENTITY" ]; then echo "NOT REPRODUCIBLE" >&2; exit 1; fi
echo "reproducible: manifest $A_MANIFEST identity $A_IDENTITY (buildkit $BUILDKIT_IMAGE)"
