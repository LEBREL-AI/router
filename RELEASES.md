# Releases and measurements

Every release of the router that serves traffic is listed here with what a third party needs to
check that the running machine is this code:

| Version | Git tag | Image digest | dstack compose hash | Since |
|---|---|---|---|---|
| 1.0.1 | `v1.0.1` | `sha256:1b0079066e90ca902c0e11db6b139dc7e40ea0aa00c9eb796ebc6741da23e574` | _(recorded at deployment)_ | 2026-11-04 |
| 1.0.0 | `v1.0.0` | `sha256:67566c5629e9541d9188feee2955034b6bb16def2ecf9d1a6585317363c9c465` | `38b091826b3ce68e832d61e95282b358104717e1825b778c12c3d231b1980c24` | 2026-11-04 |

How a release is made: tag the commit; run `scripts/reproducible-build.sh <tag>` (two clean builds,
same digest, or the release stops); push that image to `ghcr.io/lebrel-ai/router` and pin its digest
in `deploy/dstack/docker-compose.yml`; deploy the compose with dstack; record the compose hash dstack
reports and the image digest here.

How anyone checks a running router: fetch its attestation quote, verify Intel's signature chain
(`dstack-verifier` or `dcap-qvl`), read the compose hash out of the quote and compare it with the
row above; rebuild the image from the tag and compare the digest. The key the router publishes at
`/.well-known/proof-of-edition` is bound to that quote, so a receipt signed with it was signed by
this code and nothing else.
