# Releases and measurements

Every release of the router that serves traffic is listed here with what a third party needs to
check that the running machine is this code:

| Version | Git tag | Image digest | dstack compose hash | Receipt key id | Since |
|---|---|---|---|---|---|
| 1.3.2 (custom domain, instance A) | `v1.3.2` | `sha256:0c59d65c11d366a1fd8f61e49158a92c21d0baa79219b6ffda907bcc92c37179` + dstack-ingress 2.2 (`deploy/dstack/docker-compose.custom-domain.yml`, router-tee.lebrel.ai) | `e17840197f21c6eb27a55963751b67a8d2e3f3192074512919c69814b478a0f9` | receipt key id `a3f2ea0a6f8abedc10bacadc1bb0029b0795309cbf796c53b4c830e6b69fced4`, public key `f679f933d3853339767d14f5982a03ed383283b236ca1f13f6f20e7848cd1df3` (an app key derived inside the VM); the request-encryption key is born at random inside the VM per boot and per UTC day and named only by the attestation | 2026-11-04 |
| 1.3.2 | `v1.3.2` | `sha256:0c59d65c11d366a1fd8f61e49158a92c21d0baa79219b6ffda907bcc92c37179` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types; two builds identical) | `bd625f569b5fbe2cd9087bbbb862adb6caf572342868abbd6c81370c5bad51f0` (gateway compose, replica B) | receipt key id `937c1b37e23516003783af5754480f6c0aa6247fd161cdd259e9c9cbf7f40c31`, public key `3b4f987738dbbf99dbaea6ff682d7d61a6c9245d65a52a0efcc636e5ac2cf5f4` (an app key derived inside the VM); encryption key random per boot and per day, see the attestation | 2026-11-04 |
| 1.3.1 (custom domain, instance A) | `v1.3.1` | `sha256:9f8e09106cc27aca032c6563fe9025867f492061221428c501ae29c52548d75e` + dstack-ingress 2.2 (`deploy/dstack/docker-compose.custom-domain.yml`, router-tee.lebrel.ai) | `195352a13fe7c372a54355e898108bbf7434fb4066948d9a23204c7a7fa47ba7` | receipt key id `dc84f9b3976650ee72123bec501c57bbebc819d23cb680132afe07e1c07817d2`, public key `d2cd0eab4adf24c1b7c0921b30d5fe2ee82c4ffc3ae22a4f1e8252609ac10928` (derived inside, compose in the path); the request-encryption key is derived per UTC day and named by the day's attestation, so it is not fixed here; replaced by 1.3.2 the same day | 2026-11-04 |
| 1.3.1 | `v1.3.1` | `sha256:9f8e09106cc27aca032c6563fe9025867f492061221428c501ae29c52548d75e` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types; two builds identical) | `d91af75112c9dce5272de46fb013ad15177877297302f9aa69fce550625b4eb7` (gateway compose, replica B) | receipt key id `f711f517a629c650a2d1d6ad133e7a93553f11a15130bc0bbc4183e1e0ec1597`, public key `6daed4052008a1e9b48136495b9124038a7ca69d2bd2804ed35fe835671374d3` (derived inside, compose in the path); encryption key per UTC day, see the attestation; replaced by 1.3.2 the same day | 2026-11-04 |
| 1.3.0 (custom domain, instance A) | `v1.3.0` | `sha256:88f572f4830c4df1fbf29e9c36c4375760a803fcff9c3ba1499662a712a5e3e2` + dstack-ingress 2.2 (`deploy/dstack/docker-compose.custom-domain.yml`, router-tee.lebrel.ai) | `0a9e1b732dc2cffdcd4e65a3b11372da4aea0c61a470bdcf256eaae0aaf293a3` | receipt key id `fef2039efa223f3543068af08c854e9f755c559da5254b7ac6e6f35e3bdde138`, public key `ab1ef721a6aa2ae2bc093cebe92385af2e3834ac976ad852dab576b1ce825f93`; encryption key id `b14f0b47bcdfd29414518257aa11df2db23de2c2e56ea2a76ae3f2b9682245b3`, X25519 public key `a2bc21eb45e0cd177eda827f9748f0a2538a328fa0053ce0b093dd149431511d` (both derived inside, compose in the path; replaced by 1.3.1 the same day) | 2026-11-04 |
| 1.3.0 | `v1.3.0` | `sha256:88f572f4830c4df1fbf29e9c36c4375760a803fcff9c3ba1499662a712a5e3e2` (linux/amd64, Go 1.26, BuildKit v0.27.1, OCI media types) | `8a1a1bd144c5bd23f1e9dd9244b1fd0ce64f956804fe70cd42420500bfe9ab02` (gateway compose, replica B) | receipt key id `7f9adc456058e8f5505bc065d161053010c958ac496115859c7e0c7d53b976f1`, public key `012bb8944ffa04104b1bf4a23578bf670ee9554e7e84f49319f9da4ac015b8d2`; encryption key id `cb2dead9fad835a95677dac8740ddba5ecb225fe97d567d6ccd00d723a8f4809`, X25519 public key `91193f70bf5ee51cdd9ca8ace141f616d98db21a647e1a9fa1baa5914c48f212` (both derived inside, compose in the path; replaced by 1.3.1 the same day) | 2026-11-04 |
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

From 1.3.1 the quote's report data binds the receipt key, the encryption key and the verifier's
nonce each behind its length (v3); fresh quotes wait their turn in a short line instead of being
counted per address; and a client may put a salt first inside its encrypted body
(`lebrel_receipt_salt`), which the router cuts out and uses to salt the receipt's digests
(`hash_salt_scheme: client-salt-v1`), so a receipt's prompt and answer hashes are checkable by the
two ends only.

From 1.3.2 the request-encryption key is born at random inside the VM, per boot and per UTC day
(the previous day's kept one more day), and is never derived from the VM's key service: that
service derives keys per app, so a key taken from it could be derived again by any compose the
app's owner registers, whereas a key born in the VM's memory exists nowhere else and the quote
binds it. The receipt-signing key stays an app key derived inside the VM (the same for every
compose registered under the app, which is what makes old receipts verifiable): a route manifest
says with `attested: true` when its signer runs inside an attested VM, and a data note never claims
more. The API key is checked before an encrypted body is read, an encapsulation is remembered under
its account (4,096 live entries each, 50 hours, longer than a key's life) and forgotten again when
the request is refused before credit is reserved.

Two compose files are kept: `deploy/dstack/docker-compose.yml` (the machine reached through the
dstack gateway hostname; its TLS terminates at Phala's gateway, outside the quote) and
`deploy/dstack/docker-compose.custom-domain.yml` (the same router with dstack-ingress beside it,
terminating TLS for router-tee.lebrel.ai inside the VM). Each has its own compose hash; the ledger
names which one a row was deployed from; the replica on prod5 keeps the gateway compose (one ingress
per domain), so the two instances sign with different keys. A compose-only update (`phala deploy
--cvm-id … -c <compose>` without `-e`) keeps the instance's encrypted environment, verified on the
replica with a real compose change. Reference measurements of the dstack OS image, as
observed on the quotes (not an independent rebuild): `deploy/dstack/reference-measurements/`.
