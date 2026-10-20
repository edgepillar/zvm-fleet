package fleet

import "strings"

// Names is the set of container names one chain's fleet run measures against.
// The rulesets ran side by side on one machine (chain 71, dynamic plasma, now
// the default; chain 70, momentum v1, deprecated with go-zenon's master
// branch) and share this package's phases, so the container references that
// were hardcoded to chain 70 are collected here instead, and a run picks the
// profile that matches its own `-config`/`-l1`/`-eth` chain.
//
// EngineCandidates/EngineBCandidates list every name a chain's primary and
// follower execution client has ever run under, primary first: chain 70's
// output directory holds phases from before and after the op-geth→op-reth
// migration (D2), and runningContainers filters out whichever candidate isn't
// actually up. A profile with no follower (chain 71 has none) leaves NodeB and
// EngineBCandidates empty, which every caller here already treats as "absent"
// the same way a stopped container would be.
type Names struct {
	L1Pillar, L1RPC                     string
	EngineCandidates, EngineBCandidates []string
	Node, NodeB                         string
	Relayer, Bridge, Faucet             string
}

// DefaultNames is the profile a run uses when it names none: dynamic plasma.
func DefaultNames() Names { return DPNames() }

// V1Names is chain 70: momentum v1, the profile every phase and report column
// was originally written against, and what a run from before profiles were
// recorded measured. Deprecated: go-zenon's dev branch runs dynamic plasma.
func V1Names() Names {
	return Names{
		L1Pillar:          "zvm-l1-pillar",
		L1RPC:             "zvm-l1-rpc",
		EngineCandidates:  []string{"zvm-op-geth", "zvm-op-reth"},
		EngineBCandidates: []string{"zvm-op-geth-b", "zvm-op-reth-b"},
		Node:              "zvm-node",
		NodeB:             "zvm-node-b",
		Relayer:           "zvm-relayer",
		Bridge:            "zvm-bridge",
		Faucet:            "zvm-faucet",
	}
}

// DPNames is chain 71: dynamic plasma active from genesis, run on its own
// docker-compose.dp.yml stack. It has no second node, so NodeB and
// EngineBCandidates are empty — the consistency-check phase (21-follower)
// reports "no second node running" there, which is the true state, not a
// defect to work around.
func DPNames() Names {
	return Names{
		L1Pillar:         "zvm-l1-dp-pillar",
		L1RPC:            "zvm-l1-dp-rpc",
		EngineCandidates: []string{"zvm-op-geth-dp", "zvm-op-reth-dp"},
		Node:             "zvm-node-dp",
		Relayer:          "zvm-relayer-dp",
		Bridge:           "zvm-bridge-dp",
		Faucet:           "zvm-faucet-dp",
	}
}

// ZVMDevnetNames is a zvm-devnet stack (github.com/Zenoglyphs-VM/zvm-devnet):
// chain 69 with dynamic plasma, under docker compose's default container
// names for its project. It has no second node.
func ZVMDevnetNames() Names {
	return Names{
		L1Pillar:         "zvm-devnet-l1-pillar-1",
		L1RPC:            "zvm-devnet-l1-rpc-1",
		EngineCandidates: []string{"zvm-devnet-reth-1"},
		Node:             "zvm-devnet-node-1",
		Relayer:          "zvm-devnet-relayer-1",
		Bridge:           "zvm-devnet-bridge-1",
		Faucet:           "zvm-devnet-faucet-1",
	}
}

// Profile looks up a named profile ("" / "dp" / "v2" → chain 71, "devnet" → a
// zvm-devnet stack, "v1" → the deprecated chain 70), the same names bench.ps1
// uses for the chains.
func Profile(name string) (Names, bool) {
	switch name {
	case "", "dp", "v2", "default":
		return DPNames(), true
	case "devnet", "zvm-devnet":
		return ZVMDevnetNames(), true
	case "v1":
		return V1Names(), true
	default:
		return Names{}, false
	}
}

// all is every container a phase's docker-stats sampling covers, in report
// order.
func (n Names) all() []string {
	var out []string
	add := func(c string) {
		if c != "" {
			out = append(out, c)
		}
	}
	add(n.L1Pillar)
	add(n.L1RPC)
	for _, c := range n.EngineCandidates {
		add(c)
	}
	add(n.Node)
	add(n.Relayer)
	add(n.Bridge)
	add(n.Faucet)
	for _, c := range n.EngineBCandidates {
		add(c)
	}
	add(n.NodeB)
	return out
}

// engineContainers is every execution-client container this profile has ever
// run under, primary first then follower.
func (n Names) engineContainers() []string {
	out := append([]string{}, n.EngineCandidates...)
	return append(out, n.EngineBCandidates...)
}

// measuredDirs is the du targets for this profile, one entry per container
// that actually exists in it.
func (n Names) measuredDirs() []struct {
	container string
	paths     []string
} {
	var out []struct {
		container string
		paths     []string
	}
	add := func(c string, paths ...string) {
		if c != "" {
			out = append(out, struct {
				container string
				paths     []string
			}{c, paths})
		}
	}
	add(n.L1Pillar, "/root/.znn/nom", "/root/.znn/consensus", "/root/.znn/log", "/root/.znn")
	add(n.L1RPC, "/root/.znn/nom", "/root/.znn/consensus", "/root/.znn/log", "/root/.znn")
	for _, c := range n.EngineCandidates {
		addEngineDir(add, c)
	}
	for _, c := range n.EngineBCandidates {
		addEngineDir(add, c)
	}
	add(n.Node, "/data/media", "/data")
	add(n.NodeB, "/data")
	add(n.Bridge, "/data")
	return out
}

func addEngineDir(add func(c string, paths ...string), c string) {
	switch {
	case strings.Contains(c, "geth"):
		add(c, "/data/geth/chaindata/ancient", "/data/geth/chaindata")
	case strings.Contains(c, "reth"):
		add(c, "/data/static_files", "/data/db")
	}
}

// engineDirs names the dirSizes keys for one kind of engine storage, across
// every client name this profile's primary engine has ever run under. The two
// clients lay their data out differently — and the honest measure differs
// with the layout. op-geth keeps everything under chaindata with frozen
// blocks in ancient/, in ordinary files whose apparent length is what it
// stored. op-reth keeps live state in an MDBX database under db/ that is one
// **sparse** file mapped at a fixed 4 GiB: its apparent size reads
// 4,295,999,501 bytes on an empty chain and never moves, so a delta taken
// from it is always zero and only the blocks the filesystem gave it mean
// anything. Only the running client's keys are ever non-zero.
func (n Names) engineDirs(kind string) []engineDir {
	var gethPath, rethPath string
	switch kind {
	case "state":
		gethPath, rethPath = "/data/geth/chaindata", "/data/db"
	case "history":
		gethPath, rethPath = "/data/geth/chaindata/ancient", "/data/static_files"
	}
	var out []engineDir
	for _, c := range n.EngineCandidates {
		switch {
		case strings.Contains(c, "geth"):
			out = append(out, engineDir{Key: c + ":" + gethPath})
		case strings.Contains(c, "reth"):
			out = append(out, engineDir{Key: c + ":" + rethPath, Allocated: true})
		}
	}
	return out
}
