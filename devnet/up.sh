#!/bin/sh
# Brings up a local ZVM devnet from nothing: genesis for both layers, then
# every service. Needs Docker and this repository's submodules, recursively
# (each service builds against its own zvm-core), and nothing else on the host.
#
#   ./up.sh          chain 71, with dynamic plasma (explorer and API on :48000)
#   ./up.sh v1       chain 70, the version 1 ruleset (:47000); deprecated
#   ./up.sh down     stop both, keeping their data
#
# Dynamic plasma is what go-zenon's dev branch runs; the version 1 ruleset is
# go-zenon master's, kept only to compare against while it lasts. For a
# devnet to build apps on, zvm-devnet is the whole thing in one compose
# project: github.com/Zenoglyphs-VM/zvm-devnet.
#
# Genesis is written once, into nom-dp/ and l2-dp/ (nom/ and l2/), and reused
# after that. For a fresh chain: `docker compose down -v` and remove those
# directories. A devnet's genesis timestamp should be close to now: go-zenon
# schedules momentum ticks from it, and both layers must agree on the value.
set -eu
cd "$(dirname "$0")"
root=$(cd .. && pwd)
mode=${1:-dp}
compose="docker compose -f docker-compose.yml -f docker-compose.dp.yml"

git -C "$root" submodule update --init --recursive

case "$mode" in
down)
	$compose --profile dp --profile v1 down
	exit 0
	;;
dp) ;;
v1)
	echo "warning: the version 1 ruleset (chain 70) is deprecated; ./up.sh runs dynamic plasma" >&2
	;;
*)
	echo "usage: $0 [dp|v1|down]" >&2
	exit 2
	;;
esac

# The images both rulesets run, built here rather than by compose: each is
# declared on a v1 service, which a dp run does not start.
docker build -q -t go-zenon-devnet:latest "$root/go-zenon" >/dev/null
for svc in zvm-core zvm-node zvm-relayer zvm-bridge; do
	docker build -q -t "$svc:latest" "$root/$svc" >/dev/null
done

# The genesis commands are zvm-core's CLI, run from its own image.
zvm() {
	docker run --rm --user "$(id -u):$(id -g)" -v "$root:/w" -w /w/devnet zvm-core:latest "$@"
}

# The burned fee (-fee-from) is priced against the L1's supply, which
# l2-genesis would read from a running node, and none is running yet. At
# genesis it is epoch 0 and the two totals l1-genesis just wrote, ZNN's first.
anchor() { # timestamp genesis.json
	printf '%s,0,%s' "$1" "$(sed -n 's/.*"totalSupply": *\([0-9]*\).*/\1/p' "$2" | head -2 | paste -sd, -)"
}

l2flags="-evm karst -min-base-fee 1000000 -fee-from 10 -gas-fork 200 -wire-fork 200 -bridge-fee-bps 25"
if [ "$mode" = dp ] && [ ! -f nom-dp/genesis.json ]; then
	ts=$(date +%s)
	zvm l1-genesis -out nom-dp -devnet ../go-zenon/docker/devnet -chain-id 71 -name zvm-dp -timestamp "$ts"
	# shellcheck disable=SC2086
	zvm l2-genesis -out l2-dp -l1-chain-id 71 -timestamp "$ts" -fee-anchor "$(anchor "$ts" nom-dp/genesis.json)" $l2flags
fi
if [ "$mode" = v1 ] && [ ! -f nom/genesis.json ]; then
	ts=$(date +%s)
	zvm l1-genesis -out nom -devnet ../go-zenon/docker/devnet -chain-id 70 -dynamic-plasma=false -timestamp "$ts"
	# shellcheck disable=SC2086
	zvm l2-genesis -out l2 -l1-chain-id 70 -timestamp "$ts" -fee-anchor "$(anchor "$ts" nom/genesis.json)" $l2flags
fi

# `zvm-node bench setup` and `apps setup` write their deployments here; the
# node reads them, and starts without them.
mkdir -p bench

$compose --profile "$mode" up -d
