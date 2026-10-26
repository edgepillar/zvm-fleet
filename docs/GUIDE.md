# zvm-fleet guide

The ZVM's whole-system benchmark: every service at once, across as many wallets
as the chain will fund, with storage and resources measured around each phase.

A single-workload run tells you how fast one thing is. This tells you what
happens when all of them contend for the same momentums — which on this chain is
the only number that means anything, because NoM's block budget is *shared*.
A relayer, a bridge payout, a native call and a faucet drip are all competing
for the same 100 blocks per momentum, and a benchmark that runs them one at a
time never sees it.

```sh
go build ./...

zvm-fleet -config l2/rollup.json -eth http://127.0.0.1:47545 \
          -submit http://127.0.0.1:47547 -l1 http://127.0.0.1:45997 \
          -api http://127.0.0.1:47000 \
          -deployment bench/deployment.json -apps bench/apps.json \
          -out results/run-1
```

| | |
|---|---|
| `zvm-fleet` | the run: 22 phases, 100k EVM wallets, 1k fused Zenon accounts |
| `zvm-fleet report` | rebuild the tables from a finished run's phase results |
| `zvm-fleet l1-heal` | fill L1 gossip gaps once and say what it did |

Storage is sized *inside* each container, because nothing reports a data
directory over RPC. That half ships with the node — `zvm-node bench storage`,
run in the node's own image — and this harness calls it through `-probe`, which
wants a linux/amd64 build of the node binary.

## `users`: the baseline

The phases above each offer one workload at a fixed rate, which says how much a
path can carry. `zvm-fleet users` answers the other question — what the people
using the chain see on an ordinary day — by running simulated users side by side
for hours, each with a profile:

| Profile | Lane | Mean think | Does |
|---|---|---:|---|
| `light` | batch | 8 min | checks a balance, sends ZNN to a friend, the odd small swap |
| `power` | batch | 90 s | swaps both ways, farms, moves tokens |
| `nft` | batch | 2 min | mints, lists with signed orders (D28), buys other users' listings |
| `swapper` | batch | 5 s | a bot swapping ZNN/BNA both ways, three swaps in flight |
| `bridger` | batch | 10 min | withdraws ZNN to NoM and waits for the payout |
| `native` | native | 5 min | a Syrius key: native sends, vault deposits, mints |

Think times are exponential, so each user's actions arrive as a Poisson process.
A user waits for its receipt before thinking about the next action, as a wallet
does; only the swapper pipelines. Page loads are part of every mix, because the
explorer's API is served by the process that derives the chain.

```sh
zvm-fleet users -out bench/users-baseline            # 100 users, 6 h, 30 min ramp
zvm-fleet users -users 300 -label 3x -out bench/users-3x
zvm-fleet users-report -in bench/users-baseline      # rebuild USERS.md
```

Each action is streamed to `records.jsonl` and each sample to `samples.jsonl` as
it happens, so a run cut short still reports what it ran. `-pace 0.05` shrinks
every think time for a smoke test; a run with any pace but 1 is not a baseline.

## `l1-heal`, and why it exists

A devnet pillar that loses p2p for a moment stops seeing account blocks, and its
momentums go out empty while the RPC node happily accepts more. The chain does
not stop; it silently stops including anything, which reads as a throughput
collapse and is not one.

`l1-heal` re-publishes what the pillar is missing. The fleet run does it between
phases with `-heal` (on by default) and **records each gap**, because a run that
quietly healed twelve times is not the same measurement as one that healed none,
and the difference belongs in the results rather than in the operator's memory.

## Reading the numbers

Method and results are in [`zvm-docs`](https://github.com/Zenoglyphs-VM/zvm-docs): `benchmarks/BENCHMARK.md`
for how a run is set up and what the columns mean, `benchmarks/results/FLEET*.md`
for the tables. The raw per-phase JSON each run writes is hundreds of megabytes
and belongs with the run, not in a docs repo — `-out` decides where it lands, and
`results/` here is gitignored.

Keep the referenced `runs/*.json` files with each phase file when moving a run.
`zvm-fleet report` requires relative run references inside the results directory
and checks that workload, lane and wallet count match the phase reference. A
missing, unreadable, malformed or mismatched run stops the report before output;
restore its raw input rather than relying on a cached phase summary. Summaries,
distinct included senders and error counts are rebuilt even for zero-record runs.
Unknown receipt status does not become a revert; older nonzero-gas receipt evidence
keeps the node benchmark's compatibility rule. Error rows have stable key order.
Re-rendering reads local inputs and does not repeat the benchmark or call services.

Phase accounting preserves JSON number tokens and computes counter differences
without floating-point coercion or signed-width wrapping. Integral JSON decimal
and exponent forms and decimal counter strings are supported; invalid,
fractional, overflowing or decreasing cumulative counters produce `n/a`.
Legacy in-memory floating-point counters above the safe-integer range are
unusable; previously rounded captures cannot be reconstructed by re-rendering.

Storage rows remain visible when a snapshot is absent. Missing directories,
clients or index probes and incomplete engine aggregates produce `n/a` rather
than zero. A measured zero requires usable endpoints. Storage can shrink, while
a write/head/derivation counter decrease cannot establish negative usage.
New snapshots record whether the L2 head read succeeded; a legacy zero head
without this observation is unavailable. An absent historical `names.json`
keeps the original chain-70 default, while an invalid present profile stops the
report. Byte displays retain binary scaling and nearest-even rounding.

User-report L2-block and momentum windows require two usable observations at
distinct, increasing sample times. Any observed decrease, missing counter or
conflicting sample time makes that counter's window `n/a`, including a reset
followed by recovery. A duplicate observation at the same time cannot supply a
second endpoint. Separate windows and counters retain their own availability.
These counts cover the observed sample span; they do not reconstruct unobserved
window edges or a reset hidden between samples.

New sample JSON includes `known` flags for L2, indexed, follower, L1 and derived
counters, so a successful zero read differs from an unavailable read. These
flags record parsing/read success, without asserting service health or finality.
Legacy positive counters retain their existing meaning, while legacy zero is
ambiguous. Summary JSON includes `l2BlocksKnown` and `l1MomentumsKnown`; consumers
must use them before interpreting the corresponding numeric fields. This does
not alone establish resource-column or phase-readiness validity, or complete
JSONL coverage.

Before a phase, readiness requires usable relayer queue/pending counts, an L1
momentum and the applicable L2 fee observation. An unavailable or invalid input
cannot establish an empty/calm network. Legacy L1 momentums retain the fixed
fusion-price rule; dynamic-plasma momentums need an explicit valid price. A
pre-fee L2 header remains usable when the configured fee floor is zero.

The existing 1.2x fee thresholds, five-second polling interval and bounded
fallback policy remain: phases wait at most fifteen minutes; observed empty
queues with known elevated fees retain the shorter three-minute price wait.
The maximum includes the probes themselves. Unknown inputs use the full wait
budget and end as a timeout, rather than a successful readiness observation.
The phase records the last input availability, exact counters/fee strings,
attempt count, elapsed time and `settled`, `price-timeout` or `timeout` reason,
including immediate successful checks. Read-success does not assert freshness,
authentication, finality or complete network health. A timeout still permits
the existing phase fallback and remains visibly `gaveUp: true`; caller
cancellation returns before the next phase starts.

Resource capture scopes Docker stats to selected running containers; an empty
selection issues no Docker command. Foreign rows are ignored, duplicate names
or directory paths become unavailable, and malformed Docker JSON invalidates the
batch. CPU, memory, I/O and directory fields retain independent `known` flags.
Missing, invalid or explicitly unavailable fields cannot become measured zeros
when a snapshot or sample is replayed. Size conversion uses bounded exact
decimal scaling with truncation and refuses unknown units or signed-width
overflow. Docker's rounded human sizes still cannot establish exact physical
bytes or recover precision already lost in their display.

User resource tables use exact names from `names.json`, including primary
op-geth/reth alternatives in v1, DP and devnet profiles. An absent historical
profile retains v1; an invalid present profile stops the report. CPU means use
each metric's usable observations; memory peaks and queue maxima require their
own usable inputs. Primary engines observed together are combined per sample;
an unusable present component or overflow makes that aggregate unavailable for
that metric. Followers and unrelated containers are excluded.

Summary JSON exposes `resourceObservations` counts, also printed in a separate
table. A zero count means `n/a`; measured zeros retain nonzero counts. These
counts cover accepted sample rows, not complete JSONL or whole-run coverage.
Legacy numeric fields retain presence-based compatibility; captures that
already substituted zero for failed parsing cannot be reconstructed. Corrupt
or incomplete JSONL handling, remaining ratios and full-system qualification
remain separate acceptance work.

The existing Go CI job adds a six-minute race step and a three-minute fuzz step
after build/vet/tests, reusing their module/toolchain inputs. The fuzzer targets
the standard-library-only `internal/measurement` package before the broader race
step, so its build does not instrument the harness dependencies. The race step
covers both Fleet and that internal package.
The counter fuzzer runs for five seconds with one worker against an independent
exact-rational oracle. These controls do not run the whole-system benchmark.

Two mistakes this harness records because they were made: an application
workload must ask for **estimated gas** (`Gas: 0`) rather than a fixed figure,
and each workload needs **its own wallet range**. Sharing a range makes two
workloads fight over nonces and measures the fight.

Built on [`zvm-core`](https://github.com/Zenoglyphs-VM/zvm-core), [`zvm-node`](https://github.com/Zenoglyphs-VM/zvm-node) and
[`zvm-bridge`](https://github.com/Zenoglyphs-VM/zvm-bridge) — it drives all of them, which is why it sits
downstream of every one.
