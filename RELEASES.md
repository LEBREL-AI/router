# Releases and measurements

Every release of the router that serves traffic is listed here with what a third party needs to
check that the running machine is this code:

| Version | Git tag | Image digest | dstack compose hash | Receipt key id | Since |
|---|---|---|---|---|---|
| 1.3.0 | `v1.3.0` | `sha256:88f572f4830c4df1fbf29e9c36c4375760a803fcff9c3ba1499662a712a5e3e2` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types) | _(recorded at deployment)_ | _(recorded at deployment; from 1.3.0 a row also names the encryption key id the quote binds)_ | 2026-11-04 |
| 1.2.0 (custom domain, instance A) | `v1.2.0` | `sha256:de4aaa36efa8750a85ce3a90331e0259bcf6c2bfd04e5232152452a147ac58a6` + dstack-ingress 2.2 (`deploy/dstack/docker-compose.custom-domain.yml`, router-tee.lebrel.ai) | `0b5563830f207a2f2ff03042505985d7ecd96935f2f7a1f074e84b493c8cba17` | `75b981a701b150c565d05500191dbe6a83dfa0c1bcabe8752234f41067a05204`, public key `161dc88748c3d05ee654bd56285b71115c1c5dfabc5ff9967f6ffbc21287588d` (derived inside, compose in the path) | 2026-11-04 |
| 1.2.0 | `v1.2.0` | `sha256:de4aaa36efa8750a85ce3a90331e0259bcf6c2bfd04e5232152452a147ac58a6` (linux/amd64, BuildKit v0.27.1, OCI media types) | `a28254ec21ff869593b25cf2d749c15c82563be1d676af4087cea5e9603bf9bf` | `24850d5e65638769…` (derived inside, compose in the path) | 2026-11-04 |
| 1.1.0 | `v1.1.0` | `sha256:31eb44606b599012143e85ea630e80b90095b080bc65442f8f6ba7e88e0dfd5d` (linux/amd64) | `3ba7b773f9c3b844ec4f4641c7384437a383051ed8328742c1595aac6a372c58` | `19bd924d…` (derived inside, app-wide path) | 2026-11-04 |
| 1.0.2 | `v1.0.2` | `sha256:66962f2e136059cbaefed0b2ef7b7edddf756c4b4c035a6f9b0ae466f9157fc5` (linux/amd64) | `50d65ee7c8b31f316a3dbbf5060f884df6ef522dd2a4f6df5c953799e4b5c03c` | handed in (`RECEIPT_SIGNING_SEED`) | 2026-11-04 |
| 1.0.1 | `v1.0.1` | `sha256:1b0079066e90ca902c0e11db6b139dc7e40ea0aa00c9eb796ebc6741da23e574` | `8b33778884c7441d04adc00bcd19b5398084bcdf72caec5097d553af0a934d4e` | handed in; never served (restarted) | 2026-11-04 |
| 1.0.0 | `v1.0.0` | `sha256:67566c5629e9541d9188feee2955034b6bb16def2ecf9d1a6585317363c9c465` | `38b091826b3ce68e832d61e95282b358104717e1825b778c12c3d231b1980c24` | handed in; never served (restarted) | 2026-11-04 |

How a release is made: tag the commit; run `scripts/reproducible-build.sh <tag>` on a clean tree
(two clean builds with the pinned BuildKit named in the script, same manifest digest and same
compression-independent identity, or the release stops); push that image with `scripts/push-image.sh <tag>` (same builder, OCI media
types: the registry digest must equal the local one) and pin its digest in the compose; deploy the compose with dstack; record
here the compose hash dstack reports, the image digest, and the receipt key id the deployment
derives.

How anyone checks a running router: `scripts/verify-attestation.py <router> --compose-hash <row>
--app-id 4d72eae647c67ad42c972d24175d7c32ce9c871a --key-id <row> --reference deploy/dstack/reference-measurements/dstack-0.6.1.json
--compose-file deploy/dstack/<the row's compose>`.
It asks the machine for a quote with a fresh nonce, verifies Intel's chain (dcap-qvl), recomputes
every runtime event of the log and replays the log into the RTMRs, reads the compose hash, app id
and key provider from the verified log, and compares them with the row and the arguments.

What the key proves, exactly. The receipt-signing key is derived by the VM's KMS (Phala's, named
`kms` in the measured key-provider event) for this app id, with the compose hash in the
derivation path: a receipt signed with the key of a row was signed by a VM running that app under
that measured compose. The trust anchors are Intel (the quote), the KMS operator (it releases keys
only to measured VMs of the app) and the app's owner, who alone can register new composes for the
app: a later row has a different key, and the owner, through a new compose, could derive the
keys of earlier rows (the KMS derives from the app's root key and the path): a receipt is
attributable to a row only as far as the owner is trusted not to do that, which is why the owner
is named here as an anchor. Every container of a measured compose can reach the guest
agent's socket, so the claim covers the whole compose, not the router container alone. The hash
covers dstack's whole app manifest, not only the compose file: the deployment flags (public logs
off, public system info off, public attestation data on, KMS key provider) are part of it, so a
change of any of them is a new row.

Rows 1.0.0–1.0.2 were deployed with the seed handed in as an encrypted environment variable
(`RECEIPT_SIGNING_SEED`), which a human held: their receipts prove the measured compose only as
far as that human is trusted. From 1.1.0 the seed is derived inside; from 1.2.0 the derivation
includes the compose hash and an explicit seed is refused inside a VM in production.

Two compose files are kept: `deploy/dstack/docker-compose.yml` (the machine reached through the
dstack gateway hostname; its TLS terminates at Phala's gateway, outside the quote) and
`deploy/dstack/docker-compose.custom-domain.yml` (the same router with dstack-ingress beside it,
terminating TLS for router-tee.lebrel.ai inside the VM). Each has its own compose hash; the ledger
names which one a row was deployed from; the replica on prod5 keeps the gateway compose (one ingress
per domain), so the two instances sign with different keys until the domain moves to a gateway-side
setup. Reference measurements of the dstack OS image, as
observed on the quotes (not an independent rebuild): `deploy/dstack/reference-measurements/`.
