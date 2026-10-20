// Package fleet benchmarks every ZVM service across as many wallets as the
// chain will fund: both lanes, the application contracts, the bridge, the
// faucet, the media store, verification and the explorer's API — and measures,
// around every phase, what each one wrote to the L1 nodes, to op-geth and to
// the indexer.
//
// `zvm bench` measures one workload's throughput and cost. This package is the
// layer above it: it runs those workloads at wallet counts the original harness
// could not fund, adds the services that are not transactions, and puts a
// storage and resource snapshot on both sides of each phase, so every number
// can be divided by what caused it.
package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/Zenoglyphs-VM/zvm-node/apps"
	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

// Options size the phases.
type Options struct {
	Scale        []int         // EVM wallet counts for the scaling phases
	ScaleRate    float64       // offered tx/s in the scaling phases
	SaturateRate float64       // offered tx/s at the largest wallet count
	AppWallets   int           // wallets per application workload
	AppRate      float64       // offered tx/s for application workloads
	Native       int           // native wallets to onboard by fusion and drive
	NativeRate   float64       // offered tx/s on the native lane
	Deposits     int           // bridge deposits, from the genesis native wallets
	Withdrawals  int           // bridge withdrawals, one per EVM wallet
	Drips        int           // faucet requests
	Uploads      int           // media objects
	Verifies     int           // concurrent verification jobs
	Duration     time.Duration // the default run length
	APIEach      time.Duration // per endpoint and concurrency in the API ladder
}

func DefaultOptions() Options {
	return Options{
		Scale:        []int{1_000, 10_000, 100_000},
		ScaleRate:    200,
		SaturateRate: 600,
		AppWallets:   2_000,
		AppRate:      100,
		Native:       1_000,
		NativeRate:   15,
		Deposits:     1_000,
		Withdrawals:  1_000,
		Drips:        60,
		Uploads:      900,
		Verifies:     8,
		Duration:     2 * time.Minute,
		APIEach:      4 * time.Second,
	}
}

// Key ranges. Each phase draws its own wallets so no phase inherits another's
// nonces or balances — or, worse, transactions still waiting in the relayer.
// 200,000..209,999 were used by the first application run, which left
// nonce-gapped transactions queued in the relayer; they are not reused.
const (
	scaleBase    = bench.WalletEVMBase // 1000..: the scaling phases, then withdrawals and the API-under-load run
	swapBase     = 220_000             // swappers
	mintBase     = 230_000             // NFT minters
	sellerBase   = 240_000             // NFT sellers
	buyerBase    = 250_000             // NFT buyers
	farmBase     = 260_000             // farmers
	mediaBase    = 300_000             // media sessions
	nativeBase   = 5_000               // native wallets onboarded by fusion
	faucetBase   = 60_000              // fresh Zenon addresses for the faucet
	withdrawBase = 40_000              // NoM recipients of withdrawals
)

func wei(znnWhole float64) *big.Int {
	f := new(big.Float).Mul(big.NewFloat(znnWhole), big.NewFloat(1e18))
	out, _ := f.Int(nil)
	return out
}

type Env struct {
	Rollup   *rollup.Config
	Eth      *ethclient.Client
	Submit   *ethclient.Client
	Follower *ethclient.Client // the second, independent node; nil if absent
	L1       *znn.Client
	API      string
	Dex      *bench.Deployment
	Apps     *apps.Deployment
	Arts     *bench.Artifacts
	Out      string
	Probe    string // a linux build of this binary, copied into the node containers
	Repo     string // the project root, for contract sources
	Names    Names  // which chain's containers this run measures; zero value is DefaultNames
	Heal     bool   // fill L1 gossip gaps while the phases run (heal.go)
	// SampleEvery is the sampling interval inside a phase; zero is five
	// seconds. A six-hour phase at five seconds is 4,320 `docker stats` calls
	// held in memory until the phase ends, so the long runs sample less often
	// and stream each sample to disk as well (users.go).
	SampleEvery time.Duration
	Opts        Options
	Log         *slog.Logger

	http     *http.Client
	running  map[string]bool
	state    State
	onSample func(Sample)       // also sent each sample, when set
	owned    map[int][]*big.Int // NFT ids each seller holds, from the list phase
}

// State survives between invocations, so phases can be re-run one at a time.
type State struct {
	Collection common.Address   `json:"collection"`
	Onboarded  int              `json:"nativeOnboarded"`
	Owned      map[int][]string `json:"owned,omitempty"`
}

func (e *Env) statePath() string { return filepath.Join(e.Out, "state.json") }

func (e *Env) loadState() {
	raw, err := os.ReadFile(e.statePath())
	if err == nil {
		json.Unmarshal(raw, &e.state)
	}
	e.owned = map[int][]*big.Int{}
	for w, ids := range e.state.Owned {
		for _, s := range ids {
			if v, ok := new(big.Int).SetString(s, 10); ok {
				e.owned[w] = append(e.owned[w], v)
			}
		}
	}
}

func (e *Env) saveState() error {
	e.state.Owned = map[int][]string{}
	for w, ids := range e.owned {
		for _, id := range ids {
			e.state.Owned[w] = append(e.state.Owned[w], id.String())
		}
	}
	raw, err := json.MarshalIndent(e.state, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(e.statePath(), raw, 0o644)
}

// RunRef is a bench run inside a phase: where its records are, and its summary.
type RunRef struct {
	File      string         `json:"file"`
	Scenario  string         `json:"scenario"`
	Lane      string         `json:"lane"`
	Wallets   int            `json:"wallets"`
	Rate      float64        `json:"rate"`
	Duration  string         `json:"duration"`
	PrepareMS int64          `json:"prepareMs"`
	Senders   int            `json:"senders"` // distinct wallets with at least one inclusion
	From      uint64         `json:"fromBlock"`
	To        uint64         `json:"toBlock"`
	Summary   bench.Summary  `json:"summary"`
	Errors    map[string]int `json:"errors,omitempty"`
}

type PhaseRecord struct {
	ID        string         `json:"id"`
	Title     string         `json:"title"`
	StartedAt time.Time      `json:"startedAt"`
	EndedAt   time.Time      `json:"endedAt"`
	Before    *Snapshot      `json:"before"`
	After     *Snapshot      `json:"after"`
	Samples   []Sample       `json:"samples"`
	Runs      []RunRef       `json:"runs,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	Error     string         `json:"error,omitempty"`

	mu sync.Mutex
}

func (p *PhaseRecord) set(key string, v any) {
	p.mu.Lock()
	p.Data[key] = v
	p.mu.Unlock()
}

type Phase struct {
	ID    string
	Title string
	Run   func(ctx context.Context, e *Env, p *PhaseRecord) error
}

// StreamSamples appends every phase sample to <out>/samples.jsonl as it is
// taken, for runs long enough that holding them until the phase ends is a
// risk. Call it after Init.
func (e *Env) StreamSamples() (func(), error) {
	f, err := os.OpenFile(filepath.Join(e.Out, "samples.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	e.onSample = func(s Sample) {
		raw, _ := json.Marshal(s)
		f.Write(append(raw, '\n'))
	}
	return func() { f.Close() }, nil
}

// New prepares an environment: HTTP client, running containers, the probe
// copied into the node containers, and saved state.
func (e *Env) Init(ctx context.Context) error {
	e.http = bench.PooledHTTP(60 * time.Second)
	if err := os.MkdirAll(filepath.Join(e.Out, "runs"), 0o755); err != nil {
		return err
	}
	if e.Names.Node == "" {
		e.Names = DefaultNames()
	}
	running, err := runningContainers(ctx)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	e.running = running
	if err := e.installProbe(ctx); err != nil {
		return fmt.Errorf("installing the storage probe: %w", err)
	}
	// Report() reads this back to render the right container names for this
	// -out directory, without needing the flags the run started with.
	if raw, err := json.MarshalIndent(e.Names, "", " "); err == nil {
		os.WriteFile(filepath.Join(e.Out, "names.json"), raw, 0o644)
	}
	e.loadState()
	return nil
}

// RunPhases runs phases in order, each wrapped in snapshots and sampling, and
// writes one JSON file per phase. A failed phase is recorded and the next runs.
func (e *Env) RunPhases(ctx context.Context, phases []Phase) error {
	// The healer runs for the whole benchmark, not per phase: a gap opened in
	// one phase stalls the next.
	h := &healer{e: e}
	hctx, stopHeal := context.WithCancel(ctx)
	defer stopHeal()
	if e.Heal {
		go h.run(hctx)
	}
	for _, ph := range phases {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Start from a quiet chain, as `zvm bench suite` does between steps:
		// otherwise a phase measures what the last one left behind. Measured on
		// chain 71 on 2026-09-17: saturation left 8,908 queued at 24x the fusion
		// price, and the next phase would have run inside that.
		waited, gaveUp := e.settle(ctx, 15*time.Minute)
		rec := &PhaseRecord{ID: ph.ID, Title: ph.Title, StartedAt: time.Now().UTC(), Data: map[string]any{}}
		if waited > time.Second {
			rec.Data["settle"] = map[string]any{"waitedSeconds": waited.Seconds(), "gaveUp": gaveUp}
		}
		e.Log.Info("phase", "id", ph.ID, "title", ph.Title)
		rec.Before = e.snapshot(ctx)

		sctx, stop := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.sampleLoop(sctx, rec)
		}()
		err := ph.Run(ctx, e, rec)
		stop()
		wg.Wait()

		rec.After = e.snapshot(ctx)
		rec.EndedAt = time.Now().UTC()
		// Recorded rather than hidden: a phase that needed its L1 mended did
		// not run on a healthy network, and its numbers say so.
		if heals := h.take(); len(heals) > 0 {
			rec.Data["l1Heals"] = heals
		}
		if err != nil {
			rec.Error = err.Error()
			e.Log.Error("phase failed", "id", ph.ID, "err", err)
		}
		if err := e.save(rec); err != nil {
			return err
		}
		if err := e.saveState(); err != nil {
			return err
		}
		e.Log.Info("phase done", "id", ph.ID, "took", rec.EndedAt.Sub(rec.StartedAt).Round(time.Second))
	}
	return nil
}

func (e *Env) save(rec *PhaseRecord) error {
	raw, err := json.MarshalIndent(rec, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(e.Out, rec.ID+".json"), append(raw, '\n'), 0o644)
}

// run is one bench workload inside a phase.
func (e *Env) run(ctx context.Context, p *PhaseRecord, cfg bench.RunConfig) (*bench.Result, error) {
	cfg.Rollup, cfg.Eth, cfg.Submit, cfg.L1 = e.Rollup, e.Eth, e.Submit, e.L1
	cfg.Deployment, cfg.APIBase, cfg.Artifacts = e.Dex, e.API, e.Arts
	if cfg.Log == nil {
		cfg.Log = e.Log
	}
	if cfg.Duration == 0 {
		cfg.Duration = e.Opts.Duration
	}
	e.Log.Info("run", "phase", p.ID, "scenario", cfg.Scenario, "lane", cfg.Lane, "wallets", cfg.Wallets, "rate", cfg.Rate, "duration", cfg.Duration)
	res, err := bench.Run(ctx, cfg)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s-%s-%s-w%d.json", p.ID, cfg.Scenario, cfg.Lane, cfg.Wallets)
	path := filepath.Join(e.Out, "runs", name)
	if err := res.Save(path); err != nil {
		return nil, err
	}
	ref := RunRef{File: filepath.ToSlash(filepath.Join("runs", name)), Scenario: cfg.Scenario, Lane: cfg.Lane,
		Wallets: cfg.Wallets, Rate: cfg.Rate, Duration: cfg.Duration.String(), PrepareMS: res.PrepareMS,
		From: res.StartBlock, To: res.EndBlock, Summary: res.Summary, Errors: map[string]int{}}
	senders := map[int]bool{}
	for _, r := range res.Records {
		if r.IncludedMS != 0 {
			senders[r.Wallet] = true
		}
		if r.Error != "" {
			ref.Errors[errorKind(r.Error)]++
		} else if r.IncludedMS != 0 && r.Status == 0 {
			ref.Errors["reverted"]++
		}
	}
	ref.Senders = len(senders)
	p.mu.Lock()
	p.Runs = append(p.Runs, ref)
	p.mu.Unlock()
	s := res.Summary
	e.Log.Info("run done", "scenario", cfg.Scenario, "lane", cfg.Lane, "included", s.Included, "submitted", s.Submitted,
		"sustained", fmt.Sprintf("%.1f", s.Sustained), "p50", s.Latency.P50, "failed", s.Failed, "reverted", s.Reverted)
	return res, nil
}

// errorKind trims an error to its stable part, so counts group.
func errorKind(s string) string {
	for _, marker := range []string{": balance", " is below the base fee", "nonce too low", "replacement transaction", "Only one usage", "connection refused", "context deadline", "no listings left", "timeout"} {
		if i := indexOf(s, marker); i >= 0 {
			end := i + len(marker)
			start := max(0, i-40)
			return s[start:end]
		}
	}
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// settlePrice is "close enough to base to start a phase", in go-zenon's
// thousandths: 1.2x, the same threshold `zvm bench suite` uses.
const settlePrice = 1_200

// settle waits until the relayer holds nothing and, under dynamic plasma, the
// fusion price is back near its base — or max passes, which it reports.
func (e *Env) settle(ctx context.Context, max time.Duration) (time.Duration, bool) {
	start := time.Now()
	for {
		var info struct {
			Relayer struct {
				Queued        int `json:"queued"`
				PostedPending int `json:"postedPending"`
			} `json:"relayer"`
		}
		busy := false
		if e.getJSON(ctx, "/api/info", &info) == nil {
			busy = info.Relayer.Queued > 0 || info.Relayer.PostedPending > 0
		}
		var price uint64
		hot := false
		if m, err := e.L1.FrontierMomentum(ctx); err == nil {
			price = m.FusionPrice()
			// The L2's own fee market too: a phase's wallets sign with a cap
			// sized from the base fee they start at, so starting at 5x the
			// floor — where phase 01's 16.75 M-gas funding calls left chain 70
			// — makes every call reserve 5x what it will pay, and wallets that
			// could afford the transfer cannot afford the reservation.
			if h, err := e.Eth.HeaderByNumber(ctx, nil); err == nil && h.BaseFee != nil {
				floor := new(big.Int).SetUint64(e.Rollup.MinBaseFeeAt(m.Height))
				limit := new(big.Int).Div(new(big.Int).Mul(floor, big.NewInt(settlePrice)), big.NewInt(1000))
				hot = floor.Sign() > 0 && h.BaseFee.Cmp(limit) > 0
			}
		}
		if !busy && !hot && price <= settlePrice {
			return time.Since(start), false
		}
		// A backlog is worth the long wait; a price a little above the
		// threshold with nothing queued is not — chain 71 sat at 1.275x for
		// fifteen minutes with an empty relayer. Give the price what
		// `zvm bench suite` gives it.
		if !busy && time.Since(start) > 3*time.Minute {
			e.Log.Warn("starting the phase before prices settled", "fusionPrice", float64(price)/1000, "baseFeeHigh", hot)
			return time.Since(start), true
		}
		if time.Since(start) > max {
			e.Log.Warn("chain did not settle before the phase", "queued", info.Relayer.Queued,
				"posted", info.Relayer.PostedPending, "fusionPrice", float64(price)/1000, "baseFeeHigh", hot)
			return time.Since(start), true
		}
		select {
		case <-ctx.Done():
			return time.Since(start), true
		case <-time.After(5 * time.Second):
		}
	}
}
