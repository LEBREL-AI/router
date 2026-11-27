# Lebrel router

OpenAI-compatible relay to third-party inference APIs with Lebrel's prepaid billing, a
verification status per upstream and a signed route receipt per response. Go 1.26, one
vendored dependency (Tinfoil's EHBP library, MIT) for the end-to-end encryption. It never logs prompt or completion content.

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
(`GetKey`): an app key, which every VM booted from a compose registered under this app could derive
for the same path, so instances of one compose sign alike and no human ever holds it, while it is the
attestation, not the key, that names the compose that served; inside a VM in production an explicit
`RECEIPT_SIGNING_SEED` is refused, outside a VM it is the only source. And `GET /.well-known/attestation[?nonce=<hex, up to 32 bytes>]`
serves the TDX quote whose report data is `SHA-512("lebrel-router-attestation-v1" || public_key ||
nonce)`, with the event log and the app's identity (app id, compose hash, OS image hash): a
verifier checks Intel's chain on the quote, recomputes the report data from the document, recomputes
and replays the event log, reads the compose hash from the verified log and compares it with
`RELEASES.md`, and then knows that the key signing the receipts lives in this code on this hardware.
`scripts/verify-attestation.py` does those checks; `RELEASES.md` says exactly what the key proves and
who the trust anchors are (Intel, the KMS operator, the app's owner).

End-to-end encryption. Inside the VM the router also derives an HPKE key (X25519, from the KMS under
its own path with the compose hash), publishes it per route at `/m/{model}/.well-known/lebrel-encryption`
as a document signed by the receipt key, and accepts requests encrypted to it with EHBP, the protocol
the editions' runtime and the Lebrel SDKs already speak (Tinfoil's library, vendored): the body is
sealed to the machine's key, the answer, streamed or not, comes back encrypted with keys derived
from that exchange, and errors after decryption are encrypted too. The attestation's report data
binds both keys (`lebrel-router-attestation-v2`), so a client that verified the quote knows the key
it encrypts to lives in this code on this hardware; nothing between the client and the router, not
lebrel.ai's edge, not Cloudflare, not Phala's gateway, sees the prompt or the answer. `cmd/encrypted-probe`
sends one such request. Outside a VM no key is published and encrypted requests are refused in clear.

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

## Direct clients: the attested machine as the endpoint

With `DIRECT_CLIENTS=true` the router serves ordinary API clients itself, without the platform's
edge in front: an OpenAI client with the machine's name as `base_url` reaches it over a TLS
connection that ends in this process, and the API key is checked by the control plane on every
request as always. Limits stay by key and by account, as published; nothing is capped by address.
Settings: `DIRECT_HOSTS` (the names clients call the machine by, required in production; a direct
call under any other name is refused with 421, while the platform's edge, which carries the
`X-Lebrel-Edge-Authorization` header, passes under any name); `EDGE_PROXY_SECRET` or
`EDGE_PROXY_TOKEN` (the header's own value, for a machine that must never hold the platform's
control secret: the attested ones); `CLIENT_ADDRESS`, where a client's address is known from, for
the control plane's count of wrong keys (`X-Lebrel-Client-Actor`: IPv4 whole, IPv6 by its /64,
twenty wrong keys a quarter hour per address): `connection` (TLS ends here and nothing proxies in
front), `header` (the last entry of `CLIENT_IP_HEADER` when the connection comes from
`CLIENT_IP_HEADER_FROM`, the ingress's addresses), or `none` (a TCP ingress or a provider's gateway
hides it, as dstack's does: no address is told, and wrong keys are counted for nobody rather than
for every client at once). A key the control plane refused is refused here for ten seconds without
asking again; right keys are never remembered.

Listeners: `LISTEN_ADDR` (`:8090`) serves plaintext for the platform's edge and a provider's
gateway, which terminate TLS themselves (`off` for none); `LISTEN_TLS_ADDR` (`:8443`) serves TLS
ending in this process when a certificate source is configured: `TLS_CERT_FILE` + `TLS_KEY_FILE`
(re-read when the certificate file changes), or `TLS_ACME_HOSTS` with `TLS_ACME_CACHE` (and
`TLS_ACME_EMAIL`): Let's Encrypt in-process by the TLS-ALPN-01 challenge, answered on the TLS
listener itself, so the machine needs no DNS credential and the certificates' private keys and the
account key live only in that cache, on the machine's own (encrypted) disk. Where TLS ends in the
process, direct clients are served over that listener only: a plaintext call under the published
name is misdirected (421). Inside a dstack VM the name's DNS points at the provider's gateway,
which passes the TLS connection through by SNI; the cache lives on the machine's state volume.
Browsers get CORS on the completion path, with the headers they ask for.

Two instances may stand behind one name, a primary and its backup, a relay in front choosing by
their health (`deploy/dstack/relay-config.py` writes the relay's compose). In a pair
(`TLS_ACME_PAIRED=true`) each instance obtains and renews its own certificate, under its own ACME
account, and no private key of the name leaves the machine that made it. Let's Encrypt's
validation connection, which the relay hands to either of them, finds the challenge of the one
that asked: an instance asked for a challenge it does not hold looks it up at its peer
(`TLS_ACME_PEER`, the base URL of the other's plaintext listener; `/.well-known/lebrel-acme-challenge`,
authenticated with `TLS_ACME_PEER_SECRET`, the same on both). A challenge certificate is no secret of
the name: the instance shows it to whoever connects during a validation, and presenting it only
lets the peer's own order be validated. A client is served the certificate the instance holds and
never starts an order; the instance asks Let's Encrypt itself, at most every 20 minutes and only
once it has seen that a validation of the name would find it (it calls the name as Let's Encrypt
would, with a challenge of its own making), and goes on trying that every hour while it holds its
certificate, so a renewal that could not be validated is told weeks before the certificate ends.

The relay asks each instance `GET /healthz/relay` on its plaintext listener: 200, it may be sent
customers; 503, it serves but asks to be left (the operator stepped it aside, or its certificate is
about to end), and is sent customers only when no instance answers 200; 410, it is stopping; 409,
it holds no certificate for the name. The operator's switch is `/healthz/relay/drain`
(`RELAY_SWITCH_TOKEN`): `POST {"drain": true}` steps the instance aside (refused while the other
one takes no customers, unless forced), its answers close their connections so that customers who
keep one open come back through the relay, and `GET` tells what is left on it (answers in flight,
connections open), its release, its certificates, its ACME account and why its peer could not be
asked the last time it could not. The switch is kept on the state volume (`RELAY_STATE_FILE`) with
the release it was opened under (the measured compose), and an instance that does not find its own
release there starts stepped aside: a new machine, or one back from an update however it was
updated, takes customers once it was tried and opened (`POST {"drain": false}`); one that was open
and restarts by itself, on the same release, comes back open. `{"recycle": true}` only makes the
answers close their connections, which empties a relay before it is touched, and ends by itself
after fifteen minutes. A stop is told to the relay first: the instance answers the check 410 for
`DRAIN_SECONDS` while it goes on serving, then closes its listeners and lets the answers in flight
finish.

While a release is rolled out, one instance at a time, the two sign receipts with different keys
(each release derives its own). An instance asked for a receipt it does not hold fetches it from
its peer (`RECEIPT_PEERS`) and hands it over as the peer signed it, with this router's key or with
the key that peer publishes beside its manifests; it vouches for neither, and whoever reads a
receipt checks it against the keys they pin.

## Production: Alibaba Model Studio first, the labs behind

Since 6 November 2026 the routes Model Studio serves (the two DeepSeek, GLM-5.3, Kimi K3, Kimi K3.1 Pro
since 11 November, the two Qwen3.8, and Qwen4 Max since 16 November) go first to Alibaba Cloud Model Studio's copy of each model (`dialect: "alibaba"`, `DASHSCOPE_API_KEY`;
`base_url_env: ALIBABA_MODEL_STUDIO_BASE_URL` may move the endpoint to the workspace form of the
same service, and the signed manifest keeps naming the public one). The previous upstreams stay
behind as fallback: OpenRouter pinned to the lab for DeepSeek and Qwen (`dialect: "openrouter"`,
`provider.order` = the lab, `allow_fallbacks: false`), then DeepSeek's own API; Z.ai's and
Moonshot AI's own APIs for GLM-5.3 and the two Kimi. A fallback is taken on a transport error, a 5xx,
or a refusal of Lebrel's credential or credit (401, 402 and 403 fail over like a 503); each
upstream has `timeout_seconds` (default 90) to answer with headers, and `long_timeout_seconds`
instead for a request whose body reaches `long_body_bytes` (a very long context takes its host
minutes to start answering: Model Studio is given 300 seconds from 512 KiB on, and an ordinary
request still leaves a stalled host after 90). The receipt names the upstream
that served. The Alibaba dialect maps every lab's reasoning switch (`thinking: {"type"}`,
`reasoning: {"enabled"}`, `enable_thinking`) onto `enable_thinking`, forwards `reasoning_effort`
(or OpenRouter's `reasoning.effort`) within the endpoint's `reasoning_efforts` list (Model Studio
refuses any other value; the lists are what Model Studio took on 6 November 2026), drops the
effort when the switch is off (Qwen refuses the pair), and gives a host that always reasons
(`reasoning_fixed`) its lowest listed effort to a client asking for none. GLM-5.3 Flash stays at Z.ai only. An answer served by
a fallback, and a request no upstream could serve, alert the operator (`ALERT_WEBHOOK_URL`) at
most every ten minutes per route, by upstream name and reason.
When no upstream of a route takes a request and what they answered may pass in a moment (a rate limit
or an overload: 429, 503; a gateway's error: 408, 425, 500, 502, 504; a connection that broke), the
route is asked again after a wait instead of the customer being refused: half a second after the first
round, doubled each round up to three seconds, or the time the upstream names with `Retry-After`, for
up to eight seconds from the request's first call (`UPSTREAM_PATIENCE_SECONDS`; 0 for no waiting), so
the customer has an answer, one way or the other, inside the ten seconds many clients wait.
An upstream that refuses a request now and then holds nobody else back. One that names a time is left
alone by every request until that time is over (a time longer than three seconds only from its second
refusal in a row, so a single odd refusal closes the route to nobody else), and so is one that has
refused eight calls in a row while answering nobody; the route's next upstream is asked meanwhile.
An upstream that refuses while it is answering other requests is full, not refusing everyone: those
refusals count for nothing and the requests wait on their own. Once such an upstream has refused again when its time was over, a customer who comes while
it is left alone is told at once. Once 32 requests of a route are waiting to ask it again, a request
that comes to wait for the first time is told at once too (one that has already waited is never
turned away for it), so the calls a provider that refuses gets are bounded however many customers come.
A request an upstream at its limit kept unanswered gets `503 upstream_busy` with `Retry-After`, in the
router's own words: an upstream's 429 speaks of Lebrel's account with it, never of the customer's, and
is not relayed. Nothing is charged for it, and only then is the operator alerted, with the upstream's
own code for the refusal when it gives one (a rate limit and an empty balance are both a 429). A
refused credential, an unknown model and an upstream that kept silent for its whole `timeout_seconds`
are not waited for.
An upstream may list request fields it refuses (`unsupported`): Model Studio's copy of DeepSeek V4 Pro
answers 400 to `logprobs`, which DeepSeek's own API returns, so a request that asks for token
probabilities skips Model Studio and goes to the next upstream. That is neither a failure nor a
fallback, and the last upstream of a route cannot list any. The list suits only fields whose null,
false or 0 mean "not asked": such a value is forwarded as sent.
Qwen: `qwen/qwen4-max` (Alibaba's `qwen4-max`, since 16 November 2026, Model Studio alone: reasoning
at low, high or xhigh, or off), `qwen/qwen3.8-max` (Alibaba's `qwen3.8-max-0902` snapshot) and
`qwen/qwen3.8-flash`, all 1,000,000 context and 131,072 max output.


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

## What a request carries upstream

A chat completion goes to its host as the route allows, not as it came (release 1.4.3, 27 November 2026).

- **The output cap bounds everything a host writes.** The cap (`max_tokens` or `max_completion_tokens`, or the
  route's default when a request names neither) is sent under both names. Measured on every production upstream:
  together they are one cap on reasoning and answer alike; alone, Z.ai's API ignores `max_completion_tokens` and
  Alibaba's Qwen3.8 counts its reasoning outside `max_tokens`. DeepSeek's own API, the one upstream that could not
  be measured with both, is sent `max_tokens`, which it documents as covering both. A request with a small cap and
  reasoning on may therefore end inside its reasoning (`finish_reason: length`), as it does on the labs' own APIs.
- **Listed fields are forwarded** (`internal/router/fields.go`: sampling, tools, the shape of the answer, the labs'
  reasoning switches). **A feature the routes do not offer is refused** when a request asks for it: web search
  (`enable_search`, `web_search_options`, `plugins`), a code interpreter, audio, predicted outputs, tools that are
  not functions. **Any other field is left out**, and the answer says which in `X-Lebrel-Ignored-Fields`.
- **A message's content is text and images.** A part of another type (video, audio, a file) is refused: no route
  has a price for it. An image part goes on as its http(s) address or its inline data and nothing else, a text
  part as its text, a tool as its function, and a message with the fields a chat completion gives it.
- **A PDF is read in a process of its own**, with a ceiling on its memory, its stack and its processor time and a
  deadline. A file written to hurt its reader ends that process, and the request is told the PDF could not be read.
- **A key's turns are taken before a request is read**, in the Messages and Responses formats too, and the files
  one request names are bounded: 100 documents, 8 MB of document text, 60 seconds for all of them, and as many
  images as the route reads. Files are fetched from public addresses, on the ports the web is served from, within
  an allowance per account, and one PDF is read at a time for an account.
- **A host's count is read in full.** What it reports past the prompt beyond `completion_tokens` is billed as
  output, and of several usage blocks in one stream the larger counts stand. An answer whose host counted more
  than the request's envelope is charged what the host counted, up to the amount held for the request and never
  above it; when the hold did not cover it, the operator is told.
- **A call to a host outlives the customer who asked.** A host that was sent a request reads its prompt and writes
  its answer whether or not anybody waits. A whole (non-streamed) answer the customer did not wait for is read to
  its end and charged as the host counted it; a stream the customer left before it began is closed as soon as the
  host takes it, and its prompt is charged. No other upstream is asked for a customer who has left. A count that cannot
  be one (no prompt tokens, a trillion of them) is no count. An account whose prompt a host counted beyond its hold
  is held for a day at the most a prompt can be counted at, a token a byte.

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
flight finish for up to `SHUTDOWN_SECONDS` (600); any still running after that (or at another
SIGTERM during that time) is cut and settled for what it relayed, never left for the reconciler's
maximum, and reported. Behind a relay the listeners stay open for `DRAIN_SECONDS` first, the relay's
check answering 410, so no new customer finds a closed door (a SIGTERM during that wait only ends it). The backlog, its alerts, restarts and the way to add capacity are in
[docs/scaling.md](docs/scaling.md).

## Control plane

The control plane changes routed models need live on the platform: the edge serves routed models
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
router retries the hold once with the other tier. The order of a route's upstreams never
changes with the clock or the price of the moment: a later upstream serves only when the
ones before it cannot (an outage, or a request field they do not take).

```bash
go test ./...
LOCAL_API_KEYS="acct_dev_000001:leb_live_dev_key_0123456789abcdef_0123456789abcdef0123456789abcdef:5000000" \
ROUTES_FILE=routes.example.json DEEPSEEK_API_KEY=... go run ./cmd/router
```
