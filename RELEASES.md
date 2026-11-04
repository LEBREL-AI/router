# Releases and measurements

Every release of the router that serves traffic is listed here with what a third party needs to
check that the running machine is this code:

| Version | Git tag | Image digest | dstack compose hash | Since |
|---|---|---|---|---|
| _(none yet)_ | | | | |

How a release is made: tag the commit; run `scripts/reproducible-build.sh <tag>` (two clean builds,
same digest, or the release stops); push that image to `ghcr.io/lebrel-ai/router` and pin its digest
in `deploy/dstack/docker-compose.yml`; deploy the compose with dstack; record the compose hash dstack
reports and the image digest here.

How anyone checks a running router: fetch its attestation quote, verify Intel's signature chain
(`dstack-verifier` or `dcap-qvl`), read the compose hash out of the quote and compare it with the
row above; rebuild the image from the tag and compare the digest. The key the router publishes at
`/.well-known/proof-of-edition` is bound to that quote, so a receipt signed with it was signed by
this code and nothing else.
