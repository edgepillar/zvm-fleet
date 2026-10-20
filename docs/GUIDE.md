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

Two mistakes this harness records because they were made: an application
workload must ask for **estimated gas** (`Gas: 0`) rather than a fixed figure,
and each workload needs **its own wallet range**. Sharing a range makes two
workloads fight over nonces and measures the fight.

Built on [`zvm-core`](https://github.com/Zenoglyphs-VM/zvm-core), [`zvm-node`](https://github.com/Zenoglyphs-VM/zvm-node) and
[`zvm-bridge`](https://github.com/Zenoglyphs-VM/zvm-bridge) — it drives all of them, which is why it sits
downstream of every one.
