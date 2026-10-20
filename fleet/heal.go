package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/devnet"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

// The L1 healer.
//
// Each devnet is two go-zenon nodes joined by one p2p link: the pillar, which
// produces momentums, and the RPC node everything publishes to. Account blocks
// reach the pillar only by gossip over that link, and gossip is sent once. When
// the link drops under load — measured on chain 70 on 2026-09-16, where the RPC
// node fell to zero peers and resynced momentums from the pillar twice inside
// twenty minutes — every block published during the gap stays in the RPC
// node's pool and never reaches the pillar. Momentums are re-downloaded after a
// reconnect; unconfirmed account blocks are not. The pillar then refuses every
// later block of those accounts ("account-block previous block is missing"),
// the RPC node goes on counting their plasma as spent, and the relayer's
// accounts stop with "not enough plasma on account". Chain 70 produced empty
// momentums for 75 minutes that way, and a fleet run measured nothing.
//
// The blocks are valid and signed, and the pillar has an RPC endpoint on the
// devnet network, so the cure is to hand it what gossip dropped, in height
// order. That is what this does, from inside the pillar's container because the
// endpoint is not published to the host. It is a harness for a two-node devnet,
// not a protocol fix: a network with more than one path between producer and
// publishers does not lose blocks this way.

// HealEvent is one account whose gap was filled.
type HealEvent struct {
	At       time.Time `json:"at"`
	Address  string    `json:"address"`
	From     uint64    `json:"fromHeight"`
	To       uint64    `json:"toHeight"`
	Sent     int       `json:"sent"`
	Lag      uint64    `json:"lagMomentums"` // how far behind the oldest missing block's momentum was
	Error    string    `json:"error,omitempty"`
	Frontier uint64    `json:"frontier"`
}

type healer struct {
	e      *Env
	mu     sync.Mutex
	events []HealEvent
}

// healLag is how many momentums a block may wait on the RPC node before its
// absence from the pillar counts as lost rather than still in flight.
const healLag = 3

type rawBlock struct {
	Height               uint64                  `json:"height"`
	MomentumAcknowledged struct{ Height uint64 } `json:"momentumAcknowledged"`
	raw                  json.RawMessage
}

// pillarCall is a JSON-RPC call to the pillar, made from inside its container.
func (h *healer) pillarCall(ctx context.Context, out any, method string, params ...any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", h.e.Names.L1Pillar, "sh", "-c",
		"f=$(mktemp); cat > $f; wget -qO- --header=Content-Type:application/json --post-file=$f http://127.0.0.1:35997; rm -f $f")
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%s on %s: %v: %s", method, h.e.Names.L1Pillar, err, stderr.String())
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *znn.RPCError   `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s on %s: %w", method, h.e.Names.L1Pillar, err)
	}
	if env.Error != nil {
		return fmt.Errorf("%s on %s: %w", method, h.e.Names.L1Pillar, env.Error)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// heal fills one account's gap, if it has one old enough to be a loss.
func (h *healer) heal(ctx context.Context, addr znn.Address, frontier uint64) {
	var unc struct {
		Count uint64            `json:"count"`
		List  []json.RawMessage `json:"list"`
	}
	if err := h.e.L1.Call(ctx, &unc, "ledger.getUnconfirmedBlocksByAddress", addr.String(), 0, 1000); err != nil || unc.Count == 0 {
		return
	}
	blocks := make([]rawBlock, 0, len(unc.List))
	for _, r := range unc.List {
		var b rawBlock
		if json.Unmarshal(r, &b) == nil {
			b.raw = r
			blocks = append(blocks, b)
		}
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].Height < blocks[j].Height })
	var info struct {
		AccountHeight uint64 `json:"accountHeight"`
	}
	if err := h.pillarCall(ctx, &info, "ledger.getAccountInfoByAddress", addr.String()); err != nil {
		h.record(HealEvent{At: time.Now().UTC(), Address: addr.String(), Frontier: frontier, Error: err.Error()})
		return
	}
	var missing []rawBlock
	for _, b := range blocks {
		if b.Height > info.AccountHeight {
			missing = append(missing, b)
		}
	}
	// Only a gap at the pillar's own head is a loss, and only once it is old:
	// a block published a moment ago is still on its way.
	if len(missing) == 0 || missing[0].Height != info.AccountHeight+1 ||
		missing[0].MomentumAcknowledged.Height+healLag > frontier {
		return
	}
	ev := HealEvent{At: time.Now().UTC(), Address: addr.String(), From: missing[0].Height,
		To: missing[len(missing)-1].Height, Lag: frontier - missing[0].MomentumAcknowledged.Height, Frontier: frontier}
	for _, b := range missing {
		if err := h.pillarCall(ctx, nil, "ledger.publishRawTransaction", b.raw); err != nil {
			ev.Error = fmt.Sprintf("height %d: %v", b.Height, err)
			break
		}
		ev.Sent++
	}
	h.record(ev)
	h.e.Log.Warn("filled an L1 gossip gap", "address", ev.Address, "from", ev.From, "to", ev.To,
		"sent", ev.Sent, "lagMomentums", ev.Lag, "err", ev.Error)
}

func (h *healer) record(ev HealEvent) {
	h.mu.Lock()
	h.events = append(h.events, ev)
	h.mu.Unlock()
}

// take returns and clears what was healed since the last call.
func (h *healer) take() []HealEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.events
	h.events = nil
	return out
}

// services are the accounts that publish all the time: the relayer's, the
// vault, and the funders. Checked often, because a gap in them stops a lane.
func (h *healer) services(ctx context.Context) []znn.Address {
	out := []znn.Address{h.e.Rollup.Vault, devnet.Key(devnet.DevOwner).Address, devnet.Key(devnet.BenchFunder).Address}
	var info struct {
		Relayer struct {
			Accounts []string `json:"accounts"`
		} `json:"relayer"`
	}
	if h.e.getJSON(ctx, "/api/info", &info) == nil {
		for _, a := range info.Relayer.Accounts {
			if addr, err := znn.ParseAddress(a); err == nil {
				out = append(out, addr)
			}
		}
	}
	return out
}

// wallets are the Zenon keys the phases drive: native senders and depositors.
func (h *healer) wallets() []znn.Address {
	var out []znn.Address
	for i := 0; i < h.e.Opts.Native; i++ {
		out = append(out, devnet.Key(nativeBase+uint32(i)).Address)
	}
	for i := 0; i < 200; i++ {
		out = append(out, devnet.Key(devnet.BenchNative+uint32(i)).Address)
	}
	return out
}

func (h *healer) sweep(ctx context.Context, addrs []znn.Address) {
	m, err := h.e.L1.FrontierMomentum(ctx)
	if err != nil {
		return
	}
	bench.Parallel(ctx, len(addrs), 8, func(i int) error {
		h.heal(ctx, addrs[i], m.Height)
		return nil
	})
}

// run checks the service accounts every 20 s and every wallet every 2 min
// until ctx ends.
func (h *healer) run(ctx context.Context) {
	fast := time.NewTicker(20 * time.Second)
	slow := time.NewTicker(2 * time.Minute)
	defer fast.Stop()
	defer slow.Stop()
	h.sweep(ctx, append(h.services(ctx), h.wallets()...))
	for {
		select {
		case <-ctx.Done():
			return
		case <-fast.C:
			h.sweep(ctx, h.services(ctx))
		case <-slow.C:
			h.sweep(ctx, h.wallets())
		}
	}
}

// HealOnce sweeps every account the fleet drives once and returns what it
// filled. It needs no Init: only the L1 client, the API and the profile.
func (e *Env) HealOnce(ctx context.Context) ([]HealEvent, error) {
	if e.http == nil {
		e.http = bench.PooledHTTP(60 * time.Second)
	}
	if _, err := e.L1.FrontierMomentum(ctx); err != nil {
		return nil, err
	}
	h := &healer{e: e}
	h.sweep(ctx, append(h.services(ctx), h.wallets()...))
	return h.take(), nil
}
