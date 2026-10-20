# zvm-fleet

The ZVM's whole-system benchmark, and the local devnet it runs against: every
service at once, across as many wallets as the chain will fund.

NoM's block budget is shared. A relayer, a bridge payout, a native call and a
faucet drip compete for the same momentums, and a benchmark that runs them one
at a time never sees it.

## A devnet on this machine

```sh
git clone --recurse-submodules https://github.com/Zenoglyphs-VM/zvm-fleet
cd zvm-fleet/devnet && ./up.sh     # chain 71, dynamic plasma
```

`up.sh` needs Docker and nothing else. It writes genesis for both layers with
zvm-core's CLI, then builds every service from this repository's submodules
(zvm-core, zvm-node, zvm-relayer, zvm-bridge, and upstream go-zenon for the L1)
and starts them. The explorer API and `/rpc` are then on http://localhost:48000.
`./up.sh v1` starts chain 70 instead, the version 1 ruleset of go-zenon's master
branch, which is deprecated. For a devnet to build apps on rather than to
measure, [zvm-devnet](https://github.com/Zenoglyphs-VM/zvm-devnet) is the
whole stack with every app deployed.

## Run the benchmark

```sh
go build ./...
GOOS=linux GOARCH=amd64 go build -o bin/zvm-linux ./zvm-node/cmd/zvm-node   # the -probe

zvm-fleet -config devnet/l2-dp/rollup.json -eth http://127.0.0.1:48545 \
          -submit http://127.0.0.1:48547 -l1 http://127.0.0.1:46997 \
          -api http://127.0.0.1:48000 \
          -deployment devnet/bench/deployment.json -apps devnet/bench/apps.json \
          -out results/run-1
```

| | |
|---|---|
| `zvm-fleet` | the run: 22 phases, 100k EVM wallets, 1k fused Zenon accounts |
| `zvm-fleet users` | simulated users over hours, the everyday baseline |
| `zvm-fleet report` | rebuild the tables from a finished run |
| `zvm-fleet l1-heal` | fill L1 gossip gaps once, and record them |

No node is built in: the endpoint flags default to `NOM_NODE`, `ZVM_RPC`,
`ZVM_NODE`, `ZVM_RELAYER` and `ZVM_FOLLOWER_RPC` (`.env.example`).

`FLEET.md` holds the tables. Method and results are in
[zvm-docs](https://github.com/Zenoglyphs-VM/zvm-docs) under `benchmarks/`.

MIT licence. The full guide, with how it works and why:
[docs/GUIDE.md](docs/GUIDE.md).
