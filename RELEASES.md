# Releases and measurements

Every release of the router that serves traffic is listed here with what a third party needs to
check that the running machine is this code:

| Version | Git tag | Image digest | dstack compose hash | Since |
|---|---|---|---|---|
| 1.0.2 | `v1.0.2` | `sha256:66962f2e136059cbaefed0b2ef7b7edddf756c4b4c035a6f9b0ae466f9157fc5` (linux/amd64) | `50d65ee7c8b31f316a3dbbf5060f884df6ef522dd2a4f6df5c953799e4b5c03c` | 2026-11-04 |
| 1.0.1 | `v1.0.1` | `sha256:1b0079066e90ca902c0e11db6b139dc7e40ea0aa00c9eb796ebc6741da23e574` | `8b33778884c7441d04adc00bcd19b5398084bcdf72caec5097d553af0a934d4e` | 2026-11-04 |
| 1.0.0 | `v1.0.0` | `sha256:67566c5629e9541d9188feee2955034b6bb16def2ecf9d1a6585317363c9c465` | `38b091826b3ce68e832d61e95282b358104717e1825b778c12c3d231b1980c24` | 2026-11-04 |

How a release is made: tag the commit; run `scripts/reproducible-build.sh <tag>` (two clean builds,
same digest, or the release stops); push that image to `ghcr.io/lebrel-ai/router` and pin its digest
in `deploy/dstack/docker-compose.yml`; deploy the compose with dstack; record the compose hash dstack
reports and the image digest here. The hash covers dstack's whole app manifest, not only
the compose file: the deployment flags (public logs off, public system info off, public attestation
data on, KMS key provider) are part of it, so a change of any of them is a new row.

How anyone checks a running router: fetch its attestation quote, verify Intel's signature chain
(`dstack-verifier` or `dcap-qvl`), read the compose hash out of the quote and compare it with the
row above; rebuild the image from the tag and compare the digest. The key the router publishes at
`/.well-known/proof-of-edition` is bound to that quote, so a receipt signed with it was signed by
this code and nothing else.
