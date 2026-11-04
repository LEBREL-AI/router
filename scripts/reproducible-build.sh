#!/usr/bin/env bash
# Builds the router image twice from a clean cache and compares the OCI digests. Any difference is a
# bug: the image is meant to be rebuildable, bit for bit, by anyone who wants to check what runs.
#   scripts/reproducible-build.sh [tag]
set -euo pipefail
cd "$(dirname "$0")/.."
TAG="${1:-dev}"
OUT="${OUT_DIR:-dist}"
mkdir -p "$OUT"
build() {
  docker buildx build --no-cache --provenance=false --sbom=false \
    --build-arg SOURCE_DATE_EPOCH=0 \
    --output "type=oci,dest=$OUT/router-$TAG-$1.tar,rewrite-timestamp=true" . >/dev/null
  tar -xOf "$OUT/router-$TAG-$1.tar" index.json | python3 -c 'import json,sys; print(json.load(sys.stdin)["manifests"][0]["digest"])'
}
A=$(build a)
B=$(build b)
echo "build a: $A"
echo "build b: $B"
if [ "$A" != "$B" ]; then echo "NOT REPRODUCIBLE" >&2; exit 1; fi
echo "reproducible: $A"
