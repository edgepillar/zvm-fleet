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
it happens, so a run cut short can report its retained valid prefix with explicit
partial-input evidence. `-pace 0.05` shrinks
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
already substituted zero for failed parsing cannot be reconstructed. Remaining
ratios and full-system qualification remain separate acceptance work.

User-report JSONL replay refuses malformed or ambiguous completed rows, blank
rows, invalid UTF-8, duplicate object keys and unusable essential record/sample
fields. Read errors, overlong rows and invalid present user/phase metadata stop
summary generation; an existing CLI report/summary is preserved on these input
refusals. A valid final object without a newline is accepted but marked partial.
Only an unterminated final object with incomplete syntax can leave a usable
prefix. Complete invalid data is never silently discarded as a crash tail.

Summary `inputs` and the Markdown evidence section record per-file byte counts,
SHA-256, accepted/used rows, framing state and unterminated-tail offset, size,
hash and acceptance. Missing samples remain explicit partial input with unavailable
resource measurements. Original inputs are never repaired, truncated or rewritten.
Regular-file identity/size/time checks detect ordinary changes during a read;
these are per-file observations, without an atomic cross-file snapshot or proof
of run completion, authenticity or durability of the original writer.

Rows retain the 16 MiB bound and add limits of 128 nesting levels and one million
JSON tokens. Reports refuse spans requiring more than 10,000 half-hour windows
rather than allocating an unbounded gap. Counts and rates describe retained
accepted records, not unobserved actions or a completed benchmark. Remaining
ratios and whole-system qualification remain open.

Sample `errors` and snapshot diagnostics now distinguish unavailable, garbled,
incomplete or invalid Docker observations from an empty container selection.
These messages are fixed concise reasons; they do not copy command output,
stderr or endpoint credentials into the evidence.

User records and streamed samples retain their first encoding/write/flush error
and refuse later rows. Finalization still flushes an earlier accepted prefix,
closes once, and returns both flush and close failures. Records retain the
64 KiB buffer and periodic progress flush; samples flush after each row. Counts
updated after a buffered append describe accepted records, not durable storage.
There is no `fsync` or crash/power-loss qualification. A failed stream is never
automatically retried or repaired.

A user simulation creates `records.jsonl` exclusively, refusing an existing file
before it can truncate retained records. Metadata encoding/writing and final
summary failures reach the caller. Receipt and payout observers survive the
normal action deadline, but honor run cancellation and writer failure. Every
record producer, including progress/upkeep and payout observation, is joined
before final flush/close and summary replay. Exhausting the existing thirty-minute
post-run payout budget cancels and joins the remaining observers and fails the
run; their cancellation does not claim the underlying withdrawal failed or was
replaced.

`StreamSamples` returns a `func() error` close function. Configure it outside
`RunPhases`, close it after phases return, and check its result. Concurrent phase
execution/configuration and a second active sample writer are refused. Closing
the stream during active phases cancels that owner as a stream failure. Ordinary
workload errors keep the existing continue-to-next-phase policy, while the final
return value preserves those errors. Sample-stream failure stops later phases.
Phase/state save errors are joined with the original failures. The users CLI
closes samples before replay and preserves run, stream and report errors even
when it can write a useful partial report. It prints success only when all pass.

Generic Fleet sample append mode is retained. New user simulations use the
fresh-output claim below. Stream error handling alone does not prove cross-file
atomicity, whole-run completeness, arbitrary workload cooperation with
cancellation or the durability of stored bytes.

Before initializing a user simulation, call `Env.StartUserRun()`. The users CLI
does this before dialing its optional follower or running `Init`. The output
directory must be absent or empty. An exclusive `user-run.json` marker selects
one cooperating writer; existing evidence is never adopted or removed. A failed
claim may retain its incomplete marker for inspection. New records, samples,
user/profile metadata and phase files use exclusive creation. Claimed directory
and marker identity/content are rechecked at phase and file I/O boundaries.
Phase IDs must be local filenames; duplicate or existing claimed phase output is
refused before probes start. Generic Fleet rerun behavior remains separate.

Claimed user metadata, records, samples, phase files and state use an opened
directory root. Renaming the output path cannot redirect these opens into a
replacement directory. Each metadata operation closes its root; a stream holds
its own root until close. Streams check the claim before and after underlying
writes and on close. A detected change enters the existing sticky failure and
cancellation path. Already written prefixes are preserved; buffered rows are not
durable evidence. This is an I/O-boundary check, not continuous observation or
authentication against another writer.

User initialization refuses unexpected `state.json` before container discovery.
The first state save creates the file exclusively. Later saves require the same
regular file, metadata and retained-byte hash, open it without truncation, and
write through that verified descriptor. Replaced or modified state is refused;
unrelated evidence is preserved. A failed write may leave a partial owned state
file requiring inspection. State updates do not establish atomic replacement,
`fsync`, crash/power-loss recovery, cross-file commits or report-pair publication.
Other benchmark-run and retained-stream-file writer boundaries remain separate work.

`users-report` stages Markdown, summary JSON and a `users-report.json` receipt
before replacing existing outputs. It rejects reserved input names, symlinks,
raw-input aliases and shared output identities. New report files are private;
replacement retains basic permission bits. An exclusive `users-report.lock`
serializes cooperating publishers. No stale lock is automatically adopted.

Each replacement uses an opened parent root and checked file identities/bytes.
Previous report bytes are streamed to owned recovery files, without copying raw
records. The receipt commits the two output hashes/byte counts only after both
replacements have been observed. Its `published` state describes publication;
the summary's partial-input and `runCompletion` evidence remains unchanged.
Use `users-report -in <directory> -out <markdown> -verify` to check an existing
pair without rebuilding it. The default Markdown path remains `<in>/USERS.md`.
The receipt contains hashes and a random generation identifier, without paths.

Validation or staging refusal preserves existing outputs. A publication failure
rolls back only replacements still identified as this operation's files. If an
identity, parent, claim or rollback is uncertain, the command reports `UNKNOWN`
and retains its claim/recovery files for inspection. Observed unknown replacements
are preserved; publication is not silently retried. Failed staging files may
also remain for inspection. Success removes only checked owned temporary files.

Two renames are not an atomic pair replacement. Readers requiring a checked pair
must use the receipt verifier and refuse an existing publisher claim. Independent
readers of the two legacy paths can observe an intermediate state. There is no
`fsync`, crash/power-loss recovery, writer authentication or freshness proof for
the source inputs. These controls are covered by normal and CLI package race
tests. Identity checks are not an atomic compare-and-swap against noncooperating
writers. Actual Windows/filesystem deployment remains unqualified.

A randomly generated 128-bit `runId` accompanies user records, samples, profile
metadata, user metadata and phase records, including the phase's user-metadata
copy. User report replay rejects mixed, missing-with-present, malformed or
noncanonical identifier fields. It refuses multiple user phase files instead of
selecting the last filename, checks phase ID/file agreement and compares the
retained user metadata with its phase copy. Declared population entries must be
unique known profiles with bounded counts; record user/profile pairs must match
that roster. Metadata JSON rejects duplicate keys and ambiguous root documents.
Profile metadata is read once through the same stable regular-file path used for
its hash and declaration checks.

`inputs.cohort.state` is `declared-consistent` when the observed declarations
agree, or `legacy-unbound` when every observed input lacks an identifier. Missing
context files in a declared run remain explicit partial input. Existing legacy
bytes are preserved; an identifier is never invented for them. Identifiers are
correlation data, without authentication, atomic cross-file identity, stored-byte
durability or proof of a complete run. `runCompletion` stays `not-established`.

User replay rejects reversed or inconsistent enclosing/simulation bounds and
wall-clock action records outside a declared interval. JSONL timestamps have
millisecond resolution; bound comparisons use that resolution. Legacy payout
timestamps are L2 block anchors. New payout records declare
`timestampBasis: payout-observation-start`, retain separate `payoutAnchorMs`
availability and satisfy client-clock bounds. An unavailable anchor no longer
forces an unreplayable zero timestamp into a terminal payout record.
The report retains exact interval endpoints and states whether its rate basis is
`declared-simulation`, `declared-start-retained-prefix` or
`retained-record-start-span`. Declared start/end intervals retain idle time after
the last action; an unfinished interval stays partial. The legacy observed span
does not establish the full run's duration.

Minute-aligned window labels no longer add padding to rate denominators. Each
window uses its positive overlap with the report interval; zero/unknown duration
is `n/a`, with explicit total/window/profile availability flags in JSON. A final
sample exactly on a declared half-hour endpoint belongs to the last active
window. Samples outside a declared interval remain counted/hashed as accepted
input but do not affect resource/counter tables. Legacy inputs retain the earlier
extended sample-selection policy and its stated evidence limits. Other report
aggregates, cross-domain clock guarantees and full-system qualification remain
separate acceptance work.

User storage tables retain the union of directory keys at both snapshot endpoints,
including a directory absent after the run or a missing entire snapshot. Missing,
negative or explicitly unavailable values remain `n/a`; usable zero endpoints are
retained. Directory apparent size, SQLite file bytes and SQLite pages in use are
separate measures. Snapshot capture/replay retains independent file/page presence
flags; an explicit true flag cannot invent an absent scalar, and false overrides
a populated value. Legacy zero file bytes without availability remain ambiguous.
Page products and signed changes use arbitrary-precision arithmetic. This measures
declared endpoints, without establishing physical allocation, reclaim or causality.

Summary `storage` rows retain exact decimal before/after/delta operands, independent
availability and the actual retained confirmed-record denominator. A zero denominator
is `n/a`, never one. Per-record byte display uses exact rational division and binary
scaling with nearest-even rounding; these ratios do not establish unique on-chain
transactions, whole-run coverage or a service's causal byte cost.

Gas totals and action/profile means use only retained non-skipped transaction
records with an inclusion block. New receipt capture records `gasKnown`, including
a measured zero. A positive legacy gas field remains usable; absent/ambiguous zero
or explicit false remains unknown. Gas coverage exposes `gasSamples`, `gasExpected`
and `gasObservedExact`. A total or mean is available only when every retained
included transaction in that group has usable gas. Reverted included transactions
still use gas; orders, payouts, skipped or unobserved-inclusion records do not enter
this gas subset. This is not proof of all chain activity or a complete run.

`gasTotalExact` is an arbitrary-precision decimal string. Consumers must check
`gasTotalKnown`; the compatibility `gasTotal` integer additionally requires
`gasTotalUint64Known`. Exact mean numerator/denominator fields are authoritative;
the compatibility `gasMean` float requires `gasMeanFloat64Known`. Unrepresentable
compatibility values stay unavailable instead of wrapping or rounding silently.
Markdown displays available gas means using exact integer floor. Measured zero
stays visible, while missing or incomplete coverage is `n/a`.

Profile transaction counts/outcomes and per-user-hour rates exclude reads, orders
and payouts. Separate counts retain those event kinds, and nontransaction outcomes
remain in by-action rows. Actions and errors are keyed by kind as well as action
name, so a shared name cannot merge unlike events. Equal-frequency errors have a
stable kind/action/error order. Other writer boundaries and full-system
qualification remain separate acceptance work.

User timing capture/replay distinguishes `confirmKnown`, `includeKnown` and
`latencyKnown`. A measured zero retains its scalar and declaration. Missing/null
values, false/null declarations and ambiguous legacy zero remain unavailable;
true cannot invent an absent scalar. Nonzero legacy timings remain usable within
their eligible cohort. Timing field names must be canonical. Negative elapsed
request/transaction-confirmation durations are refused. Block and payout clock
offsets can be signed and are never clamped or merged with elapsed duration.

User distributions retain exact decimal min/quantiles/max and arbitrary-precision
mean numerator/denominator. Quantiles use `floor((n-1)*p)`, without interpolation;
the original lower-index policy is preserved using integer arithmetic. Owned
vectors are sorted in place without a second full vector copy. Compatibility
quantiles require `quantilesFloat64Known`; compatibility mean requires
`meanFloat64Known`. Unrepresentable compatibility values stay unavailable.
Markdown uses exact operands with nearest-even seconds rounding, showing measured
zero and `n/a` separately. The generic service-phase distribution is unchanged.

`known`, `n`, `expected`, `complete` and `basis` describe each retained timing
cohort. Transaction confirmation and submit-to-block offset use non-skipped
included records, including reverts. Page/order request cohorts include failed
requests; skipped work is excluded. Paid payout rows have a separate clock-offset
distribution. New payout delays also need an observed, representable block
anchor; missing anchors remain explicit. No block/payout clock difference proves
clock synchronization, elapsed wallet wait or finality.

Totals, profiles, actions and applicable client-clock windows retain independent
coverage. Profile page latency excludes orders; order metrics remain separate.
Payout clock offsets do not enter transaction confirmation or client-action
windows. The Timing observations table retains exact statistics and coverage;
complete coverage applies only to retained eligible rows, not the whole run.
Broader timing fuzz, controlled performance measurement, real services and
Windows capture remain unqualified.

The existing Go CI job adds a six-minute race step and a three-minute fuzz step
after build/vet/tests, reusing their module/toolchain inputs. The fuzzer targets
the standard-library-only `internal/measurement` package before the broader race
step, so its build does not instrument the harness dependencies. The race step
covers both Fleet and that internal package. A separate three-minute CLI package
race step also runs with offline dependencies and serial package builds.
The counter fuzzer runs for five seconds with one worker against an independent
exact-rational oracle. These controls do not run the whole-system benchmark.

Two mistakes this harness records because they were made: an application
workload must ask for **estimated gas** (`Gas: 0`) rather than a fixed figure,
and each workload needs **its own wallet range**. Sharing a range makes two
workloads fight over nonces and measures the fight.

Built on [`zvm-core`](https://github.com/Zenoglyphs-VM/zvm-core), [`zvm-node`](https://github.com/Zenoglyphs-VM/zvm-node) and
[`zvm-bridge`](https://github.com/Zenoglyphs-VM/zvm-bridge) — it drives all of them, which is why it sits
downstream of every one.

## Bounded timing fuzz campaigns

`fleet/user_timing_fuzz_test.go` adds three in-memory, deterministic fuzz targets:

- `FuzzUserTimingExactStatistics` compares up to 64 available/unavailable signed
  observations with independent integer-rank, unreduced-mean, binary64
  representability and rational nearest-even display oracles. It also checks the
  distribution JSON round trip. Wide sums, signed extremes, measured zero,
  missing observations, fractional means and decimal ties are seed inputs.
- `FuzzUserTimingWireCohorts` exercises up to 16 synthetic records per input.
  Raw scalar/declaration matrices cover absent/null/legacy/explicit availability,
  capture/replay and canonical field names. Independent eligibility multisets
  check total/profile/action and preselected window reducers, including failures,
  skips, unmined/reverted transactions and unknown payout anchors. It does not
  run the complete filesystem/context/scheduling path or assign real timestamps.
- `FuzzUserTimingClockArithmetic` checks signed subtraction, block-millisecond
  narrowing, payout-anchor availability and wide integer quantile ranks against
  unbounded integer arithmetic.

Each CI target runs for five seconds with one worker after the unchanged build,
vet and normal tests. Fuzz steps use the installed local toolchain, read-only
module mode and disabled module/sum network access. Existing counter fuzz and
race steps remain separate. No fixture performs per-input filesystem or service
work. Input slices are bounded before decoding; these are finite campaigns, not
exhaustive proofs, timing benchmarks, full-run completeness or platform/runtime
acceptance. Seeds also execute during ordinary `go test` and race runs.

On a failure or panic, bounded effective synthetic arguments are logged in hex
before the failure propagates. Slice reproducers are capped at 576/432 bytes;
the clock reproducer holds four little-endian uint64 words (after, before,
seconds, rank), followed by its known flag. This retains a reproducer without
per-input files or another diagnostic artifact upload.

## Unambiguous storage observations

Live storage-probe capture and snapshot replay require canonical `fileBytes`,
`pages`, `freePages` and `pageSize` fields. Duplicate object members, including
escaped duplicates and duplicates inside nested storage metadata, are refused.
Snapshot envelopes also refuse duplicate members and case aliases for the index,
follower and availability declarations. JSON structure uses the existing retained
metadata depth rule. A refused live probe supplies no storage observation and
records its error; it must not become a measured zero or a partially trusted
replacement for ambiguous data.

Canonical measured zero, absent/null scalars, explicitly unavailable values,
legacy unknown file zero, independent file/page availability and optional index
or follower data retain their prior meaning. Unique unknown extension fields
remain allowed. Synthetic POSIX-command and JSON regression fixtures exercise
these boundaries without real Docker, SQLite, project RPC, wallets or chain
operations. Output-size/time bounds, real probe integration, full run completeness,
provenance, durability and other platform/operator requirements remain separate.


### Bounded measurement subprocess capture

Container discovery, statistics, directory/storage observation and the two probe
installation commands share bounded capture: at most 1 MiB stdout and 8 KiB
stderr per direct Docker command, a 30-second command deadline (or the earlier
caller deadline), and 250 ms for inherited output pipes after command exit or
cancellation. Exact byte boundaries remain usable; either output overflow stops
the direct command and invalidates all of its stdout. The retained stderr prefix
is bounded. Exit status, caller cancellation/custom cause, deadline and pipe-wait
errors remain inspectable through wrapping. A limit stop does not claim that the
caller canceled the operation.

Directory capture keeps complete observations from other selected containers
when one command fails. Snapshot errors retain that failure; the legacy map-only
directory helper still discards its returned error for existing callers.

The controls invoke finite synthetic POSIX commands. They do not run a Docker
daemon, database, remote container, wallet or chain, establish a general process
sandbox, or prove that a canceled Docker client stops an operation inside a
daemon/container. Remote operation lifetime, whole-process-tree control, Windows,
operational restart/healing commands, complete probe/stream/retained-file coverage,
durability/freshness and independent operator acceptance remain separate gates.

Retained sample/snapshot error summaries stay concise and do not embed command
stderr. Direct errors keep a separate bounded diagnostic and inspectable
cause chain; a retained error string is not that richer error object.


### Claimed output file binding

Opened claimed output files bind the visible local filename to the created
regular file and its accepted append state. Before each write or close, the
writer checks the opened directory/marker, file identity, type, size, modification
time, mode and current append offset. After a write it checks the same filename,
the exact accepted byte delta and offset, and the run claim again. An observed
replacement, missing name, metadata or offset change is a sticky refusal; later
writes do not resume merely because the old name is restored. Close retains the
first refusal, closes both owned descriptors and is idempotent.

The record buffer and sample stream propagate these file refusals to their owner.
Static refusal controls leave observed retained and replacement files untouched
when the change is detected before writing. Small synthetic filesystem controls
exercise direct write/close and buffered record/sample paths,
plus clean and concurrent appends. Checks use constant-size metadata and the
existing bounded run marker; they do not reread the whole accumulated stream.

This is observed file binding, not content authentication, a namespace transaction
or a filesystem snapshot. Same-size content edits with restored metadata, unknown
hard-link aliases, noncooperating mutations between checks, full writer/consumer
adoption, Windows, disk-full/fsync/crash/power-loss recovery and independent
operator acceptance remain unqualified. These checks cannot recover bytes already
deleted or overwritten externally. Original raw input/run evidence and the
separate checked report-pair verification contract remain unchanged.


### JSONL producer and reader framing

Before a new encoded row reaches the output buffer, the producer applies the
existing reader's framing rules: one object, unique members, valid UTF-8, at most
16 MiB encoded payload (excluding the emitted newline), 128 container levels and at
most one million tokens. The byte check uses actual JSON output, including HTML
escaping. Reader limits and encoded bytes remain unchanged; exact byte, depth and
token boundaries still replay through the sample reader.

A refused row cancels its owner, stops subsequent rows and is never written.
Finalization still flushes earlier accepted buffered rows and retains the refusal.
Record emission does not count the refused transaction as accepted output;
sample emission preserves its earlier complete prefix. Large negative fixtures
use digest-only discard sinks rather than another payload buffer or disk copy.

These checks validate the encoded frame after standard JSON marshaling. They do
not bound caller-owned values, custom marshaler work or temporary encoding
allocations, and do not replace semantic record/sample, cohort, clock or field
availability policies. Full allocation/input bounds, all stream/consumer paths,
real services, platform/durability and independent operator acceptance remain
open. The counter/timing fuzz campaigns retain their separate finite scope.


### Bounded report metadata input

Report metadata is checked before caller decoding: `user-run.json` shares the
producer's existing 1 KiB marker ceiling, and other metadata has a 16 MiB encoded
file ceiling. All metadata shares the one-million-token budget; the existing
128-container-level, unique-member and UTF-8 checks remain in place. An oversized
selected regular file is refused before reading. The bounded stream reader also
reads at most the selected budget plus one byte to detect growth. Refused input
does not become observed/stable evidence or a successful report context, and
resource refusal leaves the caller's decode target untouched.

Exact marker/file/token boundaries, optional missing files, exact integer/zero
decoding and stable-input hashes remain supported. Read refusals preserve the
input file. Large byte fixtures are streamed once per group and reused by rename
across context roles, avoiding a separate large file copy for each role.

These are encoded-input and parser-work bounds. They do not establish a fixed
whole-process heap bound, transactional caller updates for every failure, source
authenticity, a cross-file snapshot, limits for every accumulated record/metadata
selection or custom remarshal path, real services, platform/durability or operator
acceptance. Existing JSONL, cohort, clock and report-pair rules remain separate.


### Command failure reasons and probe installation

Retained command errors keep the `command unavailable` prefix and append sorted,
stable cause tokens. The tokens distinguish `caller-canceled`,
`caller-deadline`, `operation-timeout`, `stdout-limit`, `stderr-limit`,
`not-found`, `permission-denied`, `start-failed`, `pipe-wait-timeout`,
`exit-N` and `signaled`. Joined failures retain the union of observed causes;
`deadline` and `canceled` cover older errors without caller/policy metadata.
`other` means no more specific supported cause was available, and `cause-limit`
means the bounded error-tree walk could not inspect the complete chain.
A nonzero exit does not identify a daemon outage by itself. Raw stderr,
arguments and custom cancellation messages remain outside retained reports;
direct returned errors keep their bounded diagnostics and original error chain.

Measurement and discovery commands retain their 30-second operation limit.
Installing the probe uses a separate two-minute limit for each `docker cp`;
the subsequent chmod keeps the 30-second limit. Every operation still honors
an earlier caller deadline or cancellation. The stdout limit remains 1 MiB,
the stderr limit 8 KiB and the inherited-pipe wait 250 ms. Exceeding either
stream limit fails the observation and discards all stdout even when the
direct command exits successfully. These are bounded command policies, not
proof of remote daemon cancellation or tested large-binary copy performance.

### Owned Docker and SQLite probe fixture

The dedicated Linux/amd64 CI lane builds the complete Node command from the
Fleet gitlink, with static, stripped, path-trimmed Go settings. It transfers that
actual binary through `installProbe`, initially without executable bits, into
two owned containers. The containers use a scratch image built from the small
repository fixture helper, without pulling a base image or opening network
access. The helper provides only its idle process, the probe chmod operation,
bounded file digests and fixed kernel constraint observations.

Each container has a read-only root, no capabilities, no new privileges, no
network, one CPU, 384 MiB memory without swap and 64 PIDs. The test checks the
actual cgroup v2, capability and root-mount observations. Probe and data paths
are bounded tmpfs mounts. The helper is at most 8 MiB; the compiled probe is at
most 96 MiB and each synthetic database is at most 1 MiB. The image is never
published and the binary is not downloaded as a qualification artifact.

The real `bench storage` command captures distinct primary and follower SQLite
files. Independent fixture denominators cover file bytes, page size/count,
freelist, table rows and synthetic heads; primary media includes an empty file.
Database hashes must remain unchanged after read-only probing. Cancellation
before dispatch, stale/stopped discovery, absence and corrupt SQLite must not
invent observations. Cleanup checks the exact owned IDs and labels, and confirms
that no container or image with the fixture token remains. Uncertain ownership
or cleanup fails the lane.

The build and Docker steps each have four-minute CI bounds; the Go package has
a three-minute bound, the owned scenario has two minutes plus a separate
20-second cleanup allowance, and the complete job retains fifteen minutes.
The default normal, four fuzz and two race steps remain. The Docker test has an
explicit build tag and hosted ownership opt-in; missing prerequisites fail the
selected lane. This qualifies the declared fixture only. Slow disks/large-copy
deadlines, remote daemon lifetime, Windows, full services, real chain authority,
whole-system benchmarks, durability and independent operational acceptance remain
open. No performance improvement or full V04 acceptance is inferred.

The selected fixture requires local Docker BuildKit and cgroup v2. The isolated
Docker configuration selects the local Unix socket; no external builder,
registry credentials or base-image pull is used.

The retained success record includes the actual typed probe reports, transferred
binary and database digests, and both containers' observed kernel limits,
alongside the independently constructed SQLite/media denominators.

Before the offline static build, CI caches only the already declared
`github.com/decred/dcrd/dcrec/secp256k1/v4@v4.0.1` prerequisite through the
Go proxy/checksum service. Its existing module and go.mod checksums are retained;
source go.mod/go.sum changes fail the selected step. This cache preparation has
a separate one-minute bound and does not change dependency versions.

The owned fixture uses four fresh, explicitly labelled local-driver tmpfs
volumes (two per container). This keeps the root filesystem read-only while
making the real Docker archive copy path usable. Each /tmp remains bounded to
128 MiB with execution enabled; each /data remains bounded to 16 MiB with
execution disabled. Both retain nodev/nosuid. Actual kernel filesystem type,
capacity and flags are checked together with the existing cgroup/capability
limits. The local volume driver's tmpfs is mounted by Docker; its byte cap is
verified separately from the container memory cap, without assuming that all
daemon-side copy allocations are charged to the container cgroup. Cleanup
verifies labels and creation identity before removing only these four volumes,
then checks that no owned volumes remain. It does not prune other resources.

The fixture runs each container with the numeric nonroot owner of the local
source artifacts. This lets the real chmod path operate on its own copied file
without restoring any dropped capability. Both inspected configuration and
actual nonroot process state are checked; the helper refuses an owner mismatch.
The retained success record keeps only the nonroot observation, without exposing
host usernames or numeric owner values. Installation failures use a fixed
allowlist of fixture reason codes rather than copying raw diagnostics.
