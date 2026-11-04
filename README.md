# Lebrel router

OpenAI-compatible relay to third-party inference APIs with Lebrel's prepaid billing, a
verification status per upstream and a signed route receipt per response. Standard
library only, Go 1.24. It never logs prompt or completion content.

What a route receipt proves, honestly: where the request went, when, the hashes of
what was sent and received, what the client paid, and what the last probe of that
upstream said (`proof-of-edition/watch`). It does not prove which weights answered.
Self-hosted editions keep their own serving receipts; those live in the encrypted
runtime, not here.

## What this repository proves

The router is published so that nobody has to trust it. It runs inside a confidential VM (Intel
TDX, deployed with [dstack](https://github.com/Dstack-TEE/dstack)); the CPU measures the image it
boots and signs that measurement in an attestation quote. The image is built reproducibly from this
repository (`Dockerfile`, `scripts/reproducible-build.sh`), so a third party can rebuild it from a
tag, compare the digest with the quote, and know that the code they read is the code handling
their request. `RELEASES.md` lists each served version with its measurement.

What follows from reading the code: the router keeps no prompt and no completion, writes no request
body anywhere, and its upstream keys and receipt-signing key live only inside the measured machine.

Inside the VM the router asks dstack's guest agent for two things. The receipt-signing seed is
derived by the KMS for this app id with the measured compose hash in the derivation path
(`GetKey`), so every instance of the same compose signs with the same key, a different compose
signs with a different one, and no human ever holds it; inside a VM in production an explicit
`RECEIPT_SIGNING_SEED` is refused, outside a VM it is the only source. And `GET /.well-known/attestation[?nonce=<hex, up to 32 bytes>]`
serves the TDX quote whose report data is `SHA-512("lebrel-router-attestation-v1" || public_key ||
nonce)`, with the event log and the app's identity (app id, compose hash, OS image hash): a
verifier checks Intel's chain on the quote, recomputes the report data from the document, recomputes
and replays the event log, reads the compose hash from the verified log and compares it with
`RELEASES.md`, and then knows that the key signing the receipts lives in this code on this hardware.
`scripts/verify-attestation.py` does those checks; `RELEASES.md` says exactly what the key proves and
who the trust anchors are (Intel, the KMS operator, the app's owner).

What it does not prove: what the lab at the other end (DeepSeek, Z.ai, Moonshot AI, Alibaba Cloud)
does with the request under its own policy, and which weights that lab ran. The first is their
declaration; the second is measured hourly by `proof-of-edition/watch` against the published
weights where they exist.

## How a request flows

authenticate (control plane or local keys) → concurrency slots → inspect the body
(`n=1`, output cap, usage forced on streams) → reserve credit for the envelope →
try upstreams by priority, failing over on transport errors and 408/425/429/5xx before
the first byte → mark the reservation accepted at the control plane, inside the request and
before the first byte (owed through the settlement queue if lebrel.ai cannot answer within
`ACCEPT_TIMEOUT_MS`) → stream or buffer the answer through unchanged → settle the
provider-reported usage through the queue → sign the receipt.

Responses carry `X-Request-ID`, `X-Lebrel-Upstream`, `X-Lebrel-Verification` and,
for buffered responses, `Proof-Of-Edition-Receipt` (base64 JSON). Every receipt is
also served at `GET /v1/receipts/{request_id}` for 24 hours. `GET /.well-known/proof-of-edition`
publishes the signed route manifests and the public key; `GET /v1/models` lists routes with
their upstreams, pricing and verification status.

Each route also answers under `/m/{slug}/` (slug = model id with `/` → `--`) with its own
`/readyz` and `billingContractHash`, so the platform edge can register one base URL per model.
There `/.well-known/proof-of-edition` is the bare signed manifest of that route, so a receipt
verifies like a runtime's: `poe-verify-receipt --base-url https://router.lebrel.ai/m/{slug}
--request-id req_… --public-key <hex>`. Receipts need no API key.

## Configuration

Environment: `ROUTES_FILE` (see `routes.example.json`), `LISTEN_ADDR` (`:8090`),
`CONTROL_PLANE_URL` + `CONTROL_PLANE_SECRET` (remote mode) or `LOCAL_API_KEYS`
(`account:rawKey:balanceMicrousd[:concurrency];…`, development only), `RECEIPT_SIGNING_SEED`
(base64, 32 bytes; ephemeral if unset outside production), `INSTANCE_ID`, `BOARD_SOURCE`
(file or https URL of the watch board), `BOARD_URL` (public link printed in manifests),
`EDGE_PROXY_SECRET`, `MAX_REQUEST_BYTES`. Upstream keys come from the `api_key_env`
named in the routes file. Settlement and scaling settings (`SETTLEMENT_CONCURRENCY`,
`SETTLEMENT_BATCH_SIZE`, `SETTLEMENT_ALERT_SECONDS`, `ACCEPT_TIMEOUT_MS`, `ALERT_WEBHOOK_URL`,
`ALERT_TELEGRAM_CHAT_ID`, `RECEIPT_PEERS`, `HEALTH_TOKEN`, `SHUTDOWN_SECONDS`) are in
[docs/scaling.md](docs/scaling.md).

Pricing is per route in microUSD per million tokens and already includes the fee
(`fee_bps` is informational, for receipts). The billing contract hash is the gateway's
`lebrel.billing.v2` document, so the control plane's published release for the model must
carry the same rates, `version`, `effective_at` (`…T00:00:00.000Z`), context and output limits.

## Production: DeepSeek first

Since 4 November 2026 the two DeepSeek routes and the two Qwen routes go through OpenRouter
(`dialect: "openrouter"`, `base_url` `https://openrouter.ai/api/v1`, `OPENROUTER_API_KEY`), with
the provider pinned in the forced body (`provider.order` = the lab, `allow_fallbacks: false`) so
the request still ends at the lab's own API and never at another host; the DeepSeek routes keep
DeepSeek's own API as the second upstream, taken when OpenRouter cannot answer (a transport
error, a 5xx, or OpenRouter refusing Lebrel's credential or credit: 401, 402 and 403 fail over
like a 503). The dialect maps the labs' own reasoning switches (`thinking: {"type": "disabled"}`,
`enable_thinking: false`) onto OpenRouter's `reasoning: {"enabled": false}` unless the client sent
`reasoning` itself, so a client keeps writing what the lab documents. The receipt names the
upstream (`openrouter/deepseek`, `openrouter/alibaba`) and the provider OpenRouter reports.
Qwen: `qwen/qwen3.8-max` (Alibaba's `qwen3.8-max-0902` snapshot) and `qwen/qwen3.8-flash`, both
1,000,000 context and 131,072 max output, Alibaba's list rate plus 20 %.


`routes.production.json` serves DeepSeek's two live models as-is from the lab's own API:
`deepseek/deepseek-v4.1-flash` (`deepseek-flash`, text and image input) and
`deepseek/deepseek-v4-pro` (`deepseek-v4-pro`), both 1,048,576 context and 393,216
max output, both reasoning by default at effort high (clients send
`thinking: {"type": "disabled"}` to turn it off). Prices are DeepSeek's off-peak rate
plus 5%: Flash 0.1575 / 0.63 (cache read 0.00315), Pro 0.693 / 2.079 (cache read
0.0231) USD per million, doubled inside DeepSeek's peak windows except on the Chinese
public holidays listed. `python3 scripts/platform_config.py routes.production.json
https://<router origin>` prints the three platform variables whose hashes match the
router's (`TestPrintBillingHashes`).

## Settlement is owed, never waited for

The money of a request is decided when the upstream answers; telling the control plane is
bookkeeping. The acceptance is marked inside the request, before the customer's first byte, so
the hold is never closed as unanswered while its answer is on the way; a slow or unreachable
control plane never costs the customer the answer, the acceptance is then owed like the
settlement. Only a refusal (the reservation is gone or already closed: nothing could be charged)
withholds the answer. Settlements go to a durable queue (`internal/settle`): appended to a journal in
`SETTLEMENT_DIR` (systemd's `StateDirectory`, `/var/lib/lebrel-router`) and fsynced, one flush per
group of operations, before they are attempted; attempted at once with up to
`SETTLEMENT_CONCURRENCY` calls in flight, one operation per call (lebrel.ai has no batch endpoint
for now; the router speaks [its contract](docs/platform-settlement-batch.md) should it come);
retried with jittered backoff while the control plane is slow or unreachable (one probe at a time when it keeps failing);
replayed after a restart; and done when the control plane holds them, delivered or already settled
the same way. Anything else it refuses, and anything that could never be sent, goes to `dead/` and
raises an alert. A reservation's acceptance is delivered before its settlement. One router per
directory: the queue locks it. A restart (SIGTERM) takes no new request and lets the answers in
flight finish for up to `SHUTDOWN_SECONDS` (600); any still running after that (or at a second
SIGTERM) is cut and settled for what it relayed, never left for the reconciler's maximum, and
reported. The backlog, its alerts, restarts and the way to add capacity are in
[docs/scaling.md](docs/scaling.md).

## Control plane

The control plane changes routed models need live on the platform branch
`claude/router-billing` (worktree `platform-router`): the edge serves routed models
next to the encrypted runtime (EHBP requests take the encrypted path, everything else
the routes, and the encrypted model is refused in plaintext), and settlements accept
`provider`, the upstream that served the request, stored on the usage event. The
platform already bills the exact usage inside the reserved envelope, never above the
maximum, so provider-reported counts settle without an exact prompt match.

To register a route on the platform: add the model id to `BILLED_MODEL_IDS`, its base
URL (`https://<router>/m/<slug>/v1`) to `INFERENCE_MODEL_ROUTES_JSON`, and a
`PUBLIC_MODEL_RELEASES_JSON` entry whose pricing repeats the route's three rates in all
six rate fields, `reservationSafetyBps` 15000, `missingUsagePolicy` "reserved",
`longContextThresholdTokens` = context − 1, `servedContextTokens` = `max_context_tokens`,
`servedOutputTokens` = `max_output_tokens`. `ROUTES_FILE=routes.example.json go test
./internal/router -run TestPrintBillingHashes -v` prints the hash the router will
announce; the platform computes the same one for a matching release (checked for the
three example routes: identical).

## Peak hours

DeepSeek doubles its price in fixed UTC windows on weekdays, except Chinese public
holidays. The route passes that through: `pricing.peak` in the routes file (multiplier,
weekdays, windows, holidays) is part of the billing contract, the customer pays the
lab's rate of the moment plus the fee, and the receipt names the tier. The control plane
prices with its own clock; if the two clocks sit on different sides of a boundary the
router retries the hold once with the other tier. Nothing ever leaves DeepSeek for
another host on price grounds; failover is only for outages, and only if a backup
upstream is configured.

```bash
go test ./...
LOCAL_API_KEYS="acct_dev_000001:leb_live_dev_key_0123456789abcdef_0123456789abcdef0123456789abcdef:5000000" \
ROUTES_FILE=routes.example.json DEEPSEEK_API_KEY=... go run ./cmd/router
```
