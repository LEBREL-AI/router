# Scaling routed traffic

How much routed traffic one router instance carries, where the limits are, the lever when it
saturates (another instance), and the alerts that say when to pull it.

## What changed

- **Acceptance inside the request.** The router marks the reservation accepted at lebrel.ai before
  the customer's first byte (`ACCEPT_TIMEOUT_MS`, 3 s). Only if lebrel.ai cannot answer in time is
  the acceptance queued, and the queue delivers it before the settlement. A routed hold is no longer
  "never accepted" while its answer streams (audit `audit3-plans-scale` HIGH-1, `audit3-plans-money`
  finding 3). On the platform's branch `claude/subscriptions` (58e3794) a `router-*` hold never
  accepted is closed at zero after 30 minutes and an accepted one waits 6 hours; an acceptance that
  arrives after such a close is answered 200 (`closedByReconciler`) and the settlement behind it is
  taken as the close's correction. A refusal (reservation gone or closed: nothing could be charged)
  withholds the answer with a 503.
- **Settlement queue.** Up to `SETTLEMENT_CONCURRENCY` calls in flight (64), one operation per call
  (`SETTLEMENT_BATCH_SIZE` stays at its default, 1: the platform is not building its batch endpoint
  for now; the router still speaks [its contract](platform-settlement-batch.md)), jittered
  exponential backoff per operation (0.5 s doubling to 1 min, half of it random). After 8 failed calls in a row (a batch is
  one call) it sends one call at a time: an operation never attempted goes at once (as good a probe as
  any), a retry waits for the probe spacing (a quarter to half a second at first, longer after each
  failed probe, never above 5 s); the first answer puts it back to full speed.
- **What counts as done, what goes to a human.** Delivered, or already settled the same way: the
  platform's `error.settled` names the settlement it holds, and it is the one owed (for an acceptance
  refused as closed, the settlement owed right behind it: a replay after a crash; with nothing owed
  behind it, the acceptance adds nothing to a settled reservation and is done too); or a zero
  `upstream_rejected` settlement meets the platform's own `reservation_not_found` 404 or
  `settlement_idempotency_conflict`. Any other 4xx about the operation and any operation that could
  never be sent (an unknown outcome, a negative amount: checked when it is created, when it is
  replayed and in each batch item on its own) goes to `dead/` with an alert. Retried, under the
  lagging alert: no answer, a redirect, any 404 but the platform's `reservation_not_found` (a page, a
  route not deployed, a wrong `CONTROL_PLANE_URL`, a catch-all answering with its own code), 401, 403,
  408, 425, 429, 5xx, and any answer to a batch call as a whole (it says nothing about its items).
- **On disk.** Every operation is in a journal in the settlement directory, fsynced, before anything
  attempts it; one writer appends and fsyncs once per group of lines, so a settlement costs a share
  of a disk flush, not two flushes of its own (a micro-benchmark of a flush per file and per directory
  on this Mac topped out at about 200 a second, for every instance together: macOS flushes the whole
  drive each time; the server's Linux fsync on its SSDs was not measured). The journal is rewritten
  with what is still owed once it passes 8 MiB and has doubled since its last rewrite. Reading it back
  after a crash: a last line cut short is a write nobody was told about and is dropped; any other line
  that does not read, or reads as an operation that could never be sent, goes to `dead/` with an
  alert. A dead letter is written and fsynced before the journal stops owing the operation; if it
  cannot be written, the operation (or the unreadable record, through every rewrite) stays in the
  journal for the next start. When the journal cannot take a line (a full disk), the operation is
  still delivered from memory, the first failure of the episode raises an alert, `memory_only` counts
  them and makes the health lagging, and the watch writes them again once the journal takes lines
  (never a request: a settlement arriving meanwhile pays its own line only), up to 64 per fsync, each
  once (a restart before that would lose them: the alert is there so the disk is fixed first). One
  of them that lebrel.ai refuses while `dead/` cannot take its letter stays in memory until the disk
  takes it, and the alert says it is not in `dead/`. The previous one-file-per-operation layout (and
  a dead letter moved back by hand) is taken over at start. One router per settlement directory (the
  queue locks it).
- **Restarts.** On SIGTERM the router takes no new request, keeps delivering settlements, and gives
  the answers in flight `SHUTDOWN_SECONDS` (600, nginx's `proxy_read_timeout`) to finish; the systemd
  units allow 660 s. Past that, or at once on a second SIGTERM, it cuts the rest: each one is settled
  for what it had relayed (the prompt as estimated and at least one token per event of text or
  reasoning, reasoning counted once under whichever name the lab uses, the receipt saying it is an
  estimate; an answer cut before anything reached the customer is charged nothing), never left as an
  accepted hold for the reconciler's maximum, and one alert lists every reservation cut. A customer
  who stopped reading cannot hold a cut answer: its writes get 1 s after the cut (a whole answer
  being written, already settled exactly, 5 s), then it is settled the same way. One cut while the
  lab had not answered yet gets the router's own 503 (`router_restarting`, retry) and is charged
  nothing; a customer who left first, or labs failing on their own at that instant, keep their usual
  answer and are not listed. With one instance, new requests find nothing to answer them during that
  time; with two or more, nginx sends them to the others. A stopping router makes one last check of
  its queue, so the alerts of its last seconds go out. `install.sh` and `add-instance.sh` restart one
  instance at a time and wait for each to answer `/readyz` before the next; `install.sh` loads its
  health check before it moves anything, and an install whose new binary does not answer stops
  there, puts the previous binary back for that instance and says which instance runs which binary.
- **Per open stream**, the router now holds about 0.2 MB instead of about 1.4 MB (its read buffer was
  1 MB per stream). Connection pools to lebrel.ai and to the labs are sized for hundreds of calls.

## What one instance carries

Measured on this machine with `cmd/loadtest` (below): lebrel.ai answering every call in 200 ms, the
time a call from the router's server to lebrel.ai takes; streamed answers of 20 tokens.

| Router | Instances | Offered | Settlement lag p50 / p99 | Backlog | Open streams | Memory per instance | Sustained |
| --- | --- | --- | --- | --- | --- | --- | --- |
| before (`main` 69b980b) | 1 | 10/s | 0.29 / 0.43 s | flat | 17 | 48 MB | yes |
| before | 1 | 12/s | 3.25 / 6.09 s | +2/s | 21 | 55 MB | no |
| before | 1 | 15/s | 7.77 / 14.70 s | +5/s | 26 | 63 MB | no |
| before | 2 | 20/s | 0.27 / 0.35 s | flat | 34 | 48 MB | yes |
| before | 2 | 25/s | 3.98 / 7.47 s | +5/s | 42 | 57 MB | no |
| after | 1 | 300/s | 0.20 / 0.20 s | flat | 507 | 130 MB | yes |
| after | 1 | 320/s | 0.22 / 0.24 s | flat | 536 | 137 MB | yes |
| after | 1 | 350/s | 1.62 / 3.02 s | +30/s | 587 | 151 MB | no |
| after | 2 | 600/s | 0.20 / 0.20 s | flat | 1,006 | 132 MB | yes |
| after | 2 | 640/s | 0.22 / 0.26 s | +1/s | 1,076 | 140 MB | at the edge (2 × 320) |
| after | 2 | 700/s | 1.62 / 3.04 s | +61/s | 1,192 | 155 MB | no |
| after, batches of 50 | 1 | 1,200/s | 0.21 / 0.38 s | flat | 2,013 | 456 MB | yes |
| after, 10-second answers | 1 | 150/s | 0.20 / 0.20 s | flat | 1,611 | 297 MB | yes |

Each line is 30 s of load (20 s for the 10-second answers), every request answered; "after" is the
router with its journal (an fsync per group of operations). Batches ran against the stand-in's
batch endpoint: lebrel.ai has none for now. Earlier runs on the same machine gave
the same picture: two instances with batches held 1,200/s too (0.21 / 0.33 s). Not router results:
two instances with batches at 1,600/s ran this machine out of local ports (16,384 on macOS) and
300/s of 10-second answers (about 4,200 open streams, 60,000 streamed events a second through four
processes) ran it out of CPU. The router's read buffer alone made the difference in memory: 596 MB
at 250/s before it was cut to 32 KB, 91 MB after.

What this test cannot show: the stand-in lab sends its headers 300 ms before its first token, which
hides the round trip of the acceptance made before the first byte; an answer that is not streamed
pays it in full (one call to lebrel.ai, about 0.2 s from the server). An acceptance slower than
`ACCEPT_TIMEOUT_MS` moves into the queue, two queue calls for that request instead of one, exactly
when lebrel.ai is slow. The stand-in lebrel.ai never slows down with load, and how many of
Hyperdrive's connections 64 calls in flight per instance hold was not measured.

**A short outage of lebrel.ai** (`-blip-at 10s -blip-for …`: every acceptance and settlement is
answered 503 `ledger_busy` for the outage's length, at 200 requests/s for 40 s; settlement lag
p50 / p99 / max):

| Router | 0.5 s outage (about 155 calls) | 2 s outage (about 460 calls) |
| --- | --- | --- |
| before the probing fix (e46c1e7) | 48 / 55.4 / 56.2 s, backlog 6,160, drained 43.8 s after the load | not measured |
| probing per failed call (e1a6780) | 0.20 / 0.86 / 1.60 s | 0.20 / 3.89 / 5.95 s |
| now (spacing capped at 5 s, new operations as probes; measured again after the round-6 fixes) | 0.20 / 0.94 / 1.05 s | 0.20 / 2.77 / 3.20 s |
| now, no outage | 0.20 / 0.20 / 0.21 s | |

Before, the 64 calls that failed together counted as 64 failures and the first probe waited 30 to
60 s while lebrel.ai was already answering. Then the probes went further apart after each failure,
so recovery waited up to about the outage's length again; now a settlement that arrives while the
queue probes goes at once and finds lebrel.ai back within milliseconds, and retries alone never wait
more than 5 s. The router still sustains 320/s with these changes (lag p99 0.24 s).

**A restart in the middle of a long answer** (the audit's harness: a 120 s stream, SIGTERM at +3 s):
before, the router exited at +30 s with status 1, the customer's stream broke (`unexpected EOF`) and
nothing reached the journal, so the accepted hold waited for the reconciler's reserved maximum. Now,
with the default 600 s, the stream finishes (all 120 events), its exact settlement is delivered, and
the router exits with 0 at +120 s. With `SHUTDOWN_SECONDS=10`, the stream is cut at +13 s, settled for
the 13 events it relayed (14 microUSD against 78 for the whole answer; the receipt says it is an
estimate), delivered, and the alert lists its reservation. A second SIGTERM 2 s after the first cuts
it at once: the router exits 2 s after the first signal, the stream is settled for its 5 events
(7 microUSD), and the alert says a second stop signal cut it. A customer who stops reading (headers
read, body never) no longer keeps a cut answer from its settlement: the blocked write fails 1 s
after the cut, and the answer is settled for what it relayed and listed in the alert.

Long answers are a CPU question, not a queue one: relaying a streamed event costs the router about
55 µs of CPU here (one read, one write per line, two small JSON reads), so 1,600 open 10-second
streams (32,000 events a second) took 1.6 to 1.9 cores. The production server has 6 cores shared
with other services: for thousands of long streams at once, a second server is the lever, not a
second instance on the same one.

"Sustained" means every request answered, the backlog flat, and 99 % of settlements at lebrel.ai
within 5 s of the customer's last byte. Before, the queue had 4 workers making two calls per
request, so one instance topped out at 4 ÷ (2 × 0.2 s) = 10 requests/s and past that the backlog
grew without bound (about 9/s with the 0.21 to 0.23 s per call measured from the server on
6 October). Now one call per request goes through the queue, 64 at a time: 64 ÷ 0.2 s = 320
settlements/s per instance (a batch endpoint would take the queue out of the way; the platform is
not building one for now). The settlement rate does not depend on how long an answer streams;
open streams do (requests per second × seconds per answer).

## Where the limits are, in order

1. **The router's queue**: about `SETTLEMENT_CONCURRENCY ÷ latency` per instance with single calls
   (320/s at 64 and 0.2 s). Raising the concurrency or adding an instance moves it (a batch
   endpoint would too; the platform is not building one for now).
2. **The platform's ledger**, shared by every instance: more router instances do not add to it.
   In production every charged settlement credits one revenue row; the scale audit measured that row
   at about 380 postings/s with a 2.5 ms commit (500 to 950/s estimated for Aurora), so one
   instance's 320/s is already in its range. The platform's branch `claude/subscriptions` (not
   deployed) splits it into 16 revenue rows (audit `audit3-plans-scale` HIGH-2); until that ships,
   the next step past one instance is the platform's, not another router. The platform holds a database
   connection only while one of a call's statements runs; when the platform keeps failing (5xx,
   `ledger_busy` on branch `claude/subscriptions`) the router drops to one probe at a time. Calls in
   flight add up across instances: watch Hyperdrive's connections (about 100 for the whole site)
   when adding one.
3. **The labs' limits per account**, shared by every instance, never raised by the router:
   - DeepSeek: per account (any key), 2,500 concurrent requests for `deepseek-flash`, 500 for
     `deepseek-v4-pro`, HTTP 429 past it; more on request, free
     ([rate limit](https://api-docs.deepseek.com/quick_start/rate_limit)).
   - Moonshot (Kimi K3): by the account's cumulative top-up; Tier 5 ($3,000) is 100 concurrent and
     300 requests per minute ([limits](https://platform.kimi.ai/docs/pricing/limits.md)).
   - Z.ai (GLM-5.3, GLM-5.3 Flash): shown only in the account console
     (z.ai/manage-apikey/rate-limits); the router's log of 30 Sep to 6 Oct already had 429s from the
     watch alone (1.4 % of GLM-5.3 Flash calls, 3.4 % of GLM-5.3).
   - More capacity with a lab means a higher tier or more accounts with that same lab, never another
     provider. A second account of one lab needs a key pool per upstream in the router (not built).
4. **One instance's machine**: about 55 µs of CPU per streamed event (above), about 0.2 MB per open
   stream, two sockets per stream (nginx side and lab side) against `LimitNOFILE=65536`, and one
   process already uses every core. The Namecheap server (6 cores, 32 GB) also runs lebrel-pay, the
   chain nodes and the watch. Not done yet: the router writes and flushes every line of a stream
   (an event is a `data:` line and a blank line); flushing only when nothing more is waiting to be
   read would save about one write per event without delaying any byte. Measure it before shipping.

## The lever: add an instance

Every instance is the same binary, routes file and `router.env`; each has its own queue. That is
safe because the platform's ledger is idempotent per reservation (`<reservation>:accepted` and
`<reservation>:settle` idempotency keys), a reservation's operations are created and delivered by
the instance that served it, and a retry from any instance is a replay.

An instance adds router capacity (another queue, more streams, restarts during which the other
instances keep answering), not ledger capacity (limit 2).

**On the same server** (minutes, nothing to buy; not more CPU): as root, from the bundle directory,
`./add-instance.sh 2`. It starts `lebrel-router@2` on 127.0.0.1:8091 (settlements in
`/var/lib/lebrel-router-2`, instance id `namecheap-2`) and waits for it to answer (if it does not,
it stops and disables it and says exactly what it changed), adds every instance's address to
`RECEIPT_PEERS`, puts the instance into the nginx upstream (`nginx -t` first; the old file is put
back if nginx refuses it; the last three copies are kept) and restarts the others one at a time,
each answering before the next, so nginx always has one to send to. Once, before the first extra instance: the server's site file has
`proxy_pass http://127.0.0.1:8090` and no `upstream lebrel_router` block (the script stops and says
so); convert it from the repository's `router.lebrel.ai.nginx`, keeping certbot's lines. `install.sh`
never overwrites the site once it exists, so an upgrade leaves every instance in rotation.

**On a second server** (more CPU and network, and the first server's failure no longer stops the
routes): the same bundle, the same `router.env` (control secret, `RECEIPT_SIGNING_SEED`, lab keys,
`EDGE_PROXY_SECRET`), its own `INSTANCE_ID`. In front, either nginx on the first server with the
second in the upstream over TLS (what is already paid for; the first server stays the only front),
or Cloudflare Load Balancing over both with a health check on `/readyz` (no single front; an
add-on). `RECEIPT_PEERS` then lists an address of each instance that the others can reach. A new
server needs the founder's go.

What must be the same on every instance, and why: `RECEIPT_SIGNING_SEED` (one public key, so a
receipt from any instance verifies against the one manifest; the manifest identity has no instance
in it), the routes file and `BOARD_URL` (the same manifests and billing contract hashes). What must
differ: `INSTANCE_ID` (in every receipt) and the settlement directory (the queue refuses one another
router holds). A receipt asked of the wrong instance is fetched from `RECEIPT_PEERS` and served only
if it carries this router's signature. The per-key and per-account limits the router keeps in memory
are per instance; the platform's account ceiling stays global.

## What to watch

`GET /healthz/settlements` on each instance with `Authorization: Bearer <HEALTH_TOKEN>`, its own
secret (never the edge's, which admits completions); without `HEALTH_TOKEN` the endpoint does not
exist. It answers `status` (`ok`, or `lagging` with HTTP 503), `pending`, `memory_only`,
`oldest_age_seconds`, `in_flight` against `concurrency`, `failing` (calls failed in a row), `probing`, `batching`, `dead_on_disk`, the
counters, and the instance's id. It never changes `/readyz`, which the platform reads to list the models: slow
bookkeeping does not take the routes offline. Ask each instance at its own address on the server
(127.0.0.1:8090, 8091…): through router.lebrel.ai a poll reaches one instance at random. Each
instance alerts on its own queue, and every alert starts with its instance id.

Alerts, in Spanish, as a log line `ALERT …` always and to `ALERT_WEBHOOK_URL` when set (a Telegram
bot's `sendMessage` URL with `ALERT_TELEGRAM_CHAT_ID`, a Discord or Slack webhook):

- **Busy** (first): the calls in flight have averaged 75 % of the bound over the last minute while
  they answer (none failing); nothing is late yet. This is when to add an instance. Clears under 50 %.
- **Lagging**: the oldest owed settlement is older than `SETTLEMENT_ALERT_SECONDS` (5) at two checks
  5 s apart, told at once when it is capacity (every call in use and answering: add an instance) or
  a risk (operations only in memory); a reminder every 10 minutes; "al día otra vez" when it clears.
  5 s, because the web chat waits up to 10 s for a turn's settlement and a customer's unsettled
  holds keep their concurrency slots. A delay with calls failing or slots to spare is the platform
  or the network, not capacity: everything is on disk and retried, so it is told only once the
  oldest has waited two minutes, and says so (while probing, the platform is failing and the router
  is already easing off: look at lebrel.ai, not the router). On 26 Oct 2026 a moment of packet
  loss hung one instance's settlement connection for 50 s with 1 of 8 calls in use, and the old
  rule asked for an instance; the connections to the platform are now pinged after 3 s of silence
  and closed when the ping goes unanswered, so a dead one is replaced in seconds.
- **The disk**: the journal cannot be written ("el disco no admite el diario"), once per episode,
  and when it writes again; meanwhile the lagging alert counts what is only in memory.
- **Cut at shutdown**: the reservations a restart cut after its budget or at a second stop signal,
  charged for what they relayed (nothing for those the lab had not answered yet).
- **Cannot be delivered**: every dead letter, with the request id and why (the platform's refusal
  code, a record unreadable on disk, an operation that could never be sent). Each is money for a
  human: `/var/lib/lebrel-router*/settlements/dead/*.json`; one that `dead/` could not take (the disk
  failing) is said so and comes back at the next start. After fixing the cause, move an
  operation's file back into its settlements directory and restart that instance: it is taken back
  at start and checked again.

## Settings

| Variable | Default | |
| --- | --- | --- |
| `ACCEPT_TIMEOUT_MS` | 3000 | the acceptance call before the first byte; past it, queued |
| `SETTLEMENT_CONCURRENCY` | 64 | calls in flight per instance |
| `SETTLEMENT_BATCH_SIZE` | 1 | operations per call; keep 1: lebrel.ai has no batch endpoint for now (above 1, up to 100, the router asks for it and falls back to single calls) |
| `SETTLEMENT_ALERT_SECONDS` | 5 | backlog age that counts as lagging |
| `ALERT_WEBHOOK_URL` | | where alerts are posted; checked at start (https), never logged |
| `ALERT_TELEGRAM_CHAT_ID` | | the chat, for a Telegram bot URL |
| `HEALTH_TOKEN` | | opens `/healthz/settlements` to monitors; unset, the endpoint does not exist |
| `RECEIPT_PEERS` | | every instance's base URL, comma separated |
| `SETTLEMENT_DIR` | `$STATE_DIRECTORY/settlements` | one per instance |
| `SHUTDOWN_SECONDS` | 600 | how long a restart lets the answers in flight finish before cutting them |

## The load test

`cmd/loadtest` starts the router binary it is given as real processes (one, or several behind a
round-robin balancer), a stand-in for lebrel.ai (same paths, idempotency keys and refusals as the
platform; every call answered after `-latency`, 200 ms; the batch endpoint with `-batch`, plus
`-item-cost` per item, 2 ms), and a stand-in lab (first token after 300 ms, then 20 tokens 50 ms
apart, with usage). It offers each rate for `-duration`, follows every request until the stand-in
holds its settlement, and reports the answers, the settlement lag (lebrel.ai receiving the
settlement minus the customer's last byte), the backlog's growth and the routers' memory.

```bash
go build -o /tmp/router ./cmd/router
go run ./cmd/loadtest -router /tmp/router -instances 1 -rates 200,300,350 -duration 30s
go run ./cmd/loadtest -router /tmp/router -instances 2 -rates 400,600,700 -duration 30s
go run ./cmd/loadtest -router /tmp/router -batch -env SETTLEMENT_BATCH_SIZE=50 -rates 400,800
go run ./cmd/loadtest -router /tmp/router -chunks 200 -rates 150 -duration 20s   # 10-second answers
```

The stand-in lebrel.ai never slows down under load: these are the router's numbers, not the
platform database's (limit 2 above). Everything ran on one machine, so the highest rates also
measure that machine.
