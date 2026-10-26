package fleet

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Zenoglyphs-VM/zvm-bridge/bridge"
	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/devnet"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/Zenoglyphs-VM/zvm-node/bench"
	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
)

// The user simulation.
//
// Every other phase in this package offers one workload at a fixed rate from a
// pool of wallets, which answers "how much can this path carry". It does not
// answer what the people this chain is for will see: a hundred users doing
// different things at their own pace, where a swapper's burst lands in the
// same momentum as someone's first NFT purchase and a withdrawal waits behind
// both. That is what this measures.
//
// Each simulated user is one wallet with a profile. A profile is a pace (the
// mean think time between actions, drawn from an exponential distribution so
// actions arrive as a Poisson process, the way independent people do) and a
// weighted mix of actions. A user waits for its transaction's receipt before
// it thinks about the next one, as a wallet UI does; only the high-frequency
// swapper keeps several in flight, because a bot does. Page loads are part of
// the mix, since the explorer's API is served by the same process that
// derives the chain.
//
// Offered load is therefore low — a few transactions a second for a hundred
// users, against a batch lane measured at ~200 — and that is the point: this
// is the baseline, the latency and cost of an ordinary day, which a later run
// at more users or a changed relayer is compared against.

// UserProfile is how one kind of user behaves.
type UserProfile struct {
	Name     string             `json:"name"`
	Title    string             `json:"title"`
	Lane     string             `json:"lane"`     // batch (MetaMask) or native (Syrius)
	Think    time.Duration      `json:"think"`    // mean pause between actions
	Pipeline int                `json:"pipeline"` // transactions in flight at once
	Fund     float64            `json:"fundZnn"`  // ZVM balance each wallet is kept near
	Mix      map[string]float64 `json:"mix"`      // action → weight
}

// Profiles are the kinds of user the simulation knows, in report order.
var Profiles = []UserProfile{
	{Name: "light", Title: "Light user: checks a balance, sends ZNN to a friend now and then",
		Lane: "batch", Think: 8 * time.Minute, Pipeline: 1, Fund: 3,
		Mix: map[string]float64{"browse-wallet": 45, "send-friend": 30, "send-new": 5, "swap-buy": 10, "browse-nft": 10}},
	{Name: "power", Title: "Power user: DeFi — swaps both ways, farms, moves tokens",
		Lane: "batch", Think: 90 * time.Second, Pipeline: 1, Fund: 30,
		Mix: map[string]float64{"swap-buy": 20, "swap-sell": 15, "farm-deposit": 10, "farm-harvest": 10,
			"token-send": 10, "send-friend": 10, "browse-dex": 15, "browse-wallet": 10}},
	{Name: "nft", Title: "NFT trader: mints, lists with signed orders, buys other people's listings",
		Lane: "batch", Think: 2 * time.Minute, Pipeline: 1, Fund: 30,
		Mix: map[string]float64{"nft-mint": 20, "nft-list": 25, "nft-buy": 20, "browse-nft": 35}},
	{Name: "swapper", Title: "High-frequency swapper: a bot trading ZNN/BNA both ways, three swaps in flight",
		Lane: "batch", Think: 5 * time.Second, Pipeline: 3, Fund: 60,
		Mix: map[string]float64{"swap-buy": 45, "swap-sell": 45, "browse-dex": 10}},
	{Name: "bridger", Title: "Bridger: withdraws ZNN to NoM and follows the payout",
		Lane: "batch", Think: 10 * time.Minute, Pipeline: 1, Fund: 10,
		Mix: map[string]float64{"withdraw": 25, "send-friend": 25, "browse-wallet": 50}},
	{Name: "native", Title: "Syrius user: a Zenon key on the native lane — sends, deposits through the vault, mints",
		Lane: "native", Think: 5 * time.Minute, Pipeline: 1, Fund: 3,
		Mix: map[string]float64{"native-send": 45, "native-deposit": 20, "native-mint": 15, "browse-wallet": 20}},
}

// DefaultMix is the 100-user baseline population.
const DefaultMix = "light=35,power=15,nft=15,swapper=10,bridger=10,native=15"

func profileByName(name string) (UserProfile, bool) {
	for _, p := range Profiles {
		if p.Name == name {
			return p, true
		}
	}
	return UserProfile{}, false
}

// ParseMix reads "light=35,power=15,…" into counts per profile, in the order
// Profiles lists them.
func ParseMix(s string) ([]ProfileCount, error) {
	want := map[string]int{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, n, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("mix: %q is not profile=count", part)
		}
		if _, ok := profileByName(name); !ok {
			return nil, fmt.Errorf("mix: no profile %q", name)
		}
		c, err := strconv.Atoi(n)
		if err != nil || c < 0 {
			return nil, fmt.Errorf("mix: %q is not a count", n)
		}
		want[name] += c
	}
	var out []ProfileCount
	for _, p := range Profiles {
		if want[p.Name] > 0 {
			out = append(out, ProfileCount{Profile: p, Count: want[p.Name]})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("mix: no users")
	}
	return out, nil
}

type ProfileCount struct {
	Profile UserProfile `json:"profile"`
	Count   int         `json:"count"`
}

// UserOptions size a user simulation.
type UserOptions struct {
	Label    string         // what the report calls the run, e.g. "baseline"
	Mix      []ProfileCount // users per profile
	Duration time.Duration  // from the first user's arrival to the last action
	Ramp     time.Duration  // users arrive evenly across this
	Seed     uint64         // for the think times and choices; the run is not replayable, the mix is
	Pace     float64        // multiplies every think time; zero is 1. A smoke test runs at 0.1
	// Withdraw and Deposit are the ZNN one bridge action moves. A ZNN each is
	// right on a genesised chain; on a shared devnet, where the bridge holds
	// only what people have actually deposited, they are small fractions —
	// chain 69 had 6 ZNN locked in total when this was written.
	Withdraw float64
	Deposit  float64
}

func (o UserOptions) withdrawZNN() float64 {
	if o.Withdraw <= 0 {
		return 1
	}
	return o.Withdraw
}

func (o UserOptions) depositZNN() float64 {
	if o.Deposit <= 0 {
		return 1
	}
	return o.Deposit
}

// Key ranges for the simulated users. Each profile has its own thousand, so a
// later run with more users of one kind does not shift another's wallets.
const (
	userBase         = 400_000 // + profile index × 1,000
	userNativeBase   = nativeBase + 900
	userWithdrawBase = 45_000 // NoM recipients of the bridgers' withdrawals
)

// Record is one thing one user did: a transaction, a page load, or a signed
// order posted to the node. They are streamed to records.jsonl as they finish,
// so a run that dies at hour five still has its first five hours.
type Record struct {
	T         int64  `json:"t"` // start, unix ms
	User      int    `json:"user"`
	Profile   string `json:"profile"`
	Action    string `json:"action"`
	Kind      string `json:"kind"` // tx, read, order, payout
	OK        bool   `json:"ok"`
	Hash      string `json:"hash,omitempty"`
	Block     uint64 `json:"block,omitempty"`
	GasUsed   uint64 `json:"gas,omitempty"`
	Reverted  bool   `json:"reverted,omitempty"`
	Skipped   string `json:"skipped,omitempty"`   // nothing to do, e.g. no listing to buy
	ConfirmMS int64  `json:"confirmMs,omitempty"` // submit → receipt seen, what the user waits
	IncludeMS int64  `json:"includeMs,omitempty"` // submit → the block's timestamp
	LatencyMS int64  `json:"latencyMs,omitempty"` // reads and orders: the request
	Bytes     int64  `json:"bytes,omitempty"`
	Error     string `json:"error,omitempty"`
}

type sim struct {
	jsonlOutput
	e         *Env
	o         UserOptions
	users     []*user
	start     time.Time
	end       time.Time
	followCtx context.Context // survives the action deadline, but not run cancellation

	pid       int
	lp        common.Address
	coll      common.Address
	domain    common.Hash
	blockTime sync.Map // block number → unix seconds

	txs     atomic.Int64
	fails   atomic.Int64
	wg      sync.WaitGroup // withdrawals still being followed to their payout
	pending sync.WaitGroup // pipelined receipts still being waited for
}

type user struct {
	s       *sim
	id      int
	p       UserProfile
	rng     *rand.Rand
	key     *ecdsa.PrivateKey // batch
	acct    *znn.Account      // native
	zaddr   znn.Address
	addr    common.Address
	mu      sync.Mutex // the sender's nonce
	sender  *bench.Sender
	sem     chan struct{}
	owned   []*big.Int // NFT ids this user believes it holds
	ownedMu sync.Mutex
	selling atomic.Int64 // BNA in pending sells, in whole tokens × 1e6
}

func (s *sim) emit(r *Record) {
	if err := s.append(r, false); err != nil {
		return
	}
	if r.Kind == "tx" && r.Skipped == "" {
		s.txs.Add(1)
		if !r.OK {
			s.fails.Add(1)
		}
	}
}

// UsersPhase is the simulation as a phase, so it runs inside RunPhases: the
// settle before it, the snapshots on both sides, the sampling and the L1
// healer are the same as every other phase's.
func UsersPhase(o UserOptions) Phase {
	n := 0
	for _, pc := range o.Mix {
		n += pc.Count
	}
	return Phase{
		ID:    "users-" + o.Label,
		Title: fmt.Sprintf("%d simulated users (%s) for %s", n, o.Label, o.Duration),
		Run: func(ctx context.Context, e *Env, p *PhaseRecord) error {
			return e.runUsers(ctx, p, o)
		},
	}
}

func (e *Env) runUsers(ctx context.Context, p *PhaseRecord, o UserOptions) (result error) {
	f, err := os.OpenFile(filepath.Join(e.Out, "records.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	s := &sim{e: e, o: o, jsonlOutput: newJSONLOutput(f, cancel), followCtx: ctx}
	defer func() {
		if err := s.finish(); err != nil && !errors.Is(result, err) {
			result = errors.Join(result, err)
		}
	}()
	meta := map[string]any{"label": o.Label, "mix": o.Mix, "duration": o.Duration.String(), "ramp": o.Ramp.String(), "pace": o.Pace,
		"chainId": e.Rollup.L2ChainID, "l1ChainId": e.Rollup.L1ChainID}
	p.set("users", meta)
	raw, err := json.MarshalIndent(meta, "", " ")
	if err != nil {
		return fmt.Errorf("encoding user metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(e.Out, "users.json"), raw, 0o644); err != nil {
		return fmt.Errorf("writing user metadata: %w", err)
	}

	prepStart := time.Now()
	if err := s.prepare(ctx, p); err != nil {
		return fmt.Errorf("preparing users: %w", err)
	}
	p.set("prepareSeconds", time.Since(prepStart).Seconds())
	return s.runSimulation(ctx, p)
}

// runSimulation joins every producer before it closes the record stream.
func (s *sim) runSimulation(ctx context.Context, p *PhaseRecord) error {
	e, o := s.e, s.o
	s.start = time.Now()
	s.end = s.start.Add(o.Duration)
	p.set("simStart", s.start.UTC())
	e.Log.Info("users arriving", "users", len(s.users), "ramp", o.Ramp, "until", s.end.Format(time.RFC3339))

	simCtx, cancel := context.WithDeadline(ctx, s.end)
	defer cancel()
	var wg sync.WaitGroup
	order := rand.New(rand.NewPCG(o.Seed, 1)).Perm(len(s.users)) // profiles interleave as they arrive
	for k, i := range order {
		u := s.users[i]
		arrive := time.Duration(0)
		if len(order) > 1 {
			arrive = o.Ramp * time.Duration(k) / time.Duration(len(order)-1)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-simCtx.Done():
				return
			case <-time.After(arrive):
			}
			u.live(simCtx)
		}()
	}
	upkeep := make(chan struct{})
	go func() { defer close(upkeep); s.upkeep(simCtx, p) }()
	progress := make(chan struct{})
	go func() { defer close(progress); s.progress(simCtx) }()

	wg.Wait()
	<-upkeep
	<-progress
	s.pending.Wait() // bounded by the receipt timeout
	p.set("simEnd", time.Now().UTC())
	// Withdrawals already initiated are followed to their payout, bounded:
	// a payout that never comes is a finding, not a reason to wait forever.
	e.Log.Info("users done; following open withdrawals to their payout")
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	followTimer := time.NewTimer(30 * time.Minute)
	defer followTimer.Stop()
	select {
	case <-done:
	case <-followTimer.C:
		e.Log.Warn("withdrawals still unpaid 30 minutes after the run")
		s.cancel(errors.New("withdrawal follow-up budget exhausted"))
		<-done
	case <-ctx.Done():
		<-done
	}
	if err := s.finish(); err != nil {
		return errors.Join(err, context.Cause(ctx))
	}
	sum, err := SummarizeUsers(e.Out)
	if err == nil {
		p.set("summary", sum.Totals)
	}
	return errors.Join(err, context.Cause(ctx))
}

func (s *sim) progress(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.flush(); err != nil {
			return
		}
		s.e.Log.Info("users progress", "elapsed", time.Since(s.start).Round(time.Minute),
			"txs", s.txs.Load(), "failed", s.fails.Load())
	}
}

// --- preparation ---------------------------------------------------------------

func (s *sim) prepare(ctx context.Context, p *PhaseRecord) error {
	e := s.e
	var err error
	if s.pid, s.lp, err = e.farmPool(ctx); err != nil {
		return err
	}
	if s.coll, err = e.collection(ctx); err != nil {
		return err
	}
	s.domain = orderbook.Domain(e.Rollup.L2ChainID, e.Apps.Exchange)

	id := 0
	for _, pc := range s.o.Mix {
		base := uint32(userBase + profileIndex(pc.Profile.Name)*1_000)
		if pc.Profile.Lane == "native" {
			base = userNativeBase
		}
		for i := 0; i < pc.Count; i++ {
			u := &user{s: s, id: id, p: pc.Profile, sem: make(chan struct{}, max(pc.Profile.Pipeline, 1)),
				rng: rand.New(rand.NewPCG(s.o.Seed, uint64(id)+1000))}
			if pc.Profile.Lane == "native" {
				k := devnet.Key(base + uint32(i))
				u.acct = znn.NewAccount(k, e.L1, e.Rollup.L1ChainID)
				u.acct.AssumeFused = true
				u.zaddr, u.addr = k.Address, common.Address(k.Address)
			} else {
				u.key = devnet.EVMKey(base + uint32(i))
				u.addr = crypto.PubkeyToAddress(u.key.PublicKey)
			}
			s.users = append(s.users, u)
			id++
		}
	}

	// Native users cannot publish without plasma, and on a shared devnet only
	// part of the key range is fused already.
	for _, pc := range s.o.Mix {
		if pc.Profile.Lane != "native" {
			continue
		}
		if err := e.onboardNativeAt(ctx, p, userNativeBase, pc.Count, 50); err != nil {
			return fmt.Errorf("fusing plasma for the native users: %w", err)
		}
	}
	// Funding, tokens and approvals, a profile at a time, through the same
	// preparation every bench run uses.
	for _, pc := range s.o.Mix {
		if err := s.fund(ctx, p, pc, true); err != nil {
			return fmt.Errorf("%s: %w", pc.Profile.Name, err)
		}
	}
	if err := bench.Parallel(ctx, len(s.users), 16, func(i int) error {
		u := s.users[i]
		if u.key == nil {
			return nil
		}
		return u.resync(ctx)
	}); err != nil {
		return err
	}
	// Per-profile one-time setup that the profile's own actions need: a
	// power user's LP position and farm approval, an NFT trader's approval of
	// the exchange. Sent from each user's own wallet, waited for once.
	return bench.Parallel(ctx, len(s.users), 16, func(i int) error { return s.users[i].setup(ctx) })
}

func profileIndex(name string) int {
	for i, p := range Profiles {
		if p.Name == name {
			return i
		}
	}
	return len(Profiles)
}

// fund brings a profile's wallets up to their balance: ZVM ZNN for gas and
// value, BNA for the ones that trade it, and, for native users, ZNN on NoM to
// burn and deposit. first also approves the router for BNA.
func (s *sim) fund(ctx context.Context, p *PhaseRecord, pc ProfileCount, first bool) error {
	e := s.e
	pr := pc.Profile
	needsToken := pr.Mix["swap-sell"] > 0 || pr.Mix["token-send"] > 0 || pr.Mix["farm-deposit"] > 0
	cfg := bench.RunConfig{Scenario: "fund", Custom: &bench.Scenario{Name: "fund", NeedsToken: first && needsToken,
		NeedsApproval: first && needsToken}, Lane: pr.Lane, Wallets: pc.Count, Fund: wei(pr.Fund),
		Duration: time.Nanosecond, Drain: time.Second, Log: e.Log}
	cfg.Rollup, cfg.Eth, cfg.Submit, cfg.L1 = e.Rollup, e.Eth, e.Submit, e.L1
	cfg.Deployment, cfg.APIBase, cfg.Artifacts = e.Dex, e.API, e.Arts
	if pr.Lane == "native" {
		cfg.NativeBase, cfg.NativeLimit = userNativeBase, pc.Count
	} else {
		cfg.WalletBase = uint32(userBase + profileIndex(pr.Name)*1_000)
	}
	if _, err := bench.Run(ctx, cfg); err != nil {
		return err
	}
	if pr.Lane == "native" {
		addrs := make([]znn.Address, pc.Count)
		for i := range addrs {
			addrs[i] = devnet.Key(userNativeBase + uint32(i)).Address
		}
		// ZNN on NoM, for the vault deposits and — where the chain prices
		// native calls (D9) — for the fee they burn. Twenty deposits' worth is
		// past what one user makes in a six-hour run.
		want := new(big.Float).Mul(big.NewFloat(20*s.o.depositZNN()), new(big.Float).SetInt(znn.OneZNN))
		amount, _ := want.Int(nil)
		return e.fundNativeZNNAt(ctx, p, addrs, userNativeBase, amount)
	}
	return nil
}

func (u *user) setup(ctx context.Context) error {
	s := u.s
	e := s.e
	switch {
	case u.p.Mix["farm-deposit"] > 0:
		router := e.Arts.MustGet("UniswapV2Router02")
		add, err := router.Call("addLiquidityETH", e.Dex.ERC20, wei(1000), new(big.Int), new(big.Int), u.addr, deadline())
		if err != nil {
			return err
		}
		approveFarm, err := e.Arts.MustGet("BenchERC20").Call("approve", e.Apps.Farm, maxUint256)
		if err != nil {
			return err
		}
		lp := s.lp
		u.mu.Lock()
		_, err = u.sender.Send(ctx, &e.Dex.Router, wei(1), add, 400_000)
		var last *types.Transaction
		if err == nil {
			last, err = u.sender.Send(ctx, &lp, new(big.Int), approveFarm, 80_000)
		}
		u.mu.Unlock()
		if err != nil {
			return err
		}
		_, err = bench.WaitReceipt(ctx, e.Eth, last, 10*time.Minute)
		return err
	case u.p.Mix["nft-list"] > 0:
		data, err := e.Arts.MustGet("ZVMCollection").Call("setApprovalForAll", e.Apps.Exchange, true)
		if err != nil {
			return err
		}
		coll := s.coll
		u.mu.Lock()
		tx, err := u.sender.Send(ctx, &coll, new(big.Int), data, 80_000)
		u.mu.Unlock()
		if err != nil {
			return err
		}
		_, err = bench.WaitReceipt(ctx, e.Eth, tx, 10*time.Minute)
		return err
	}
	return nil
}

// resync rebuilds the sender from the relayer's view of the nonce. A dropped
// transaction leaves a gap every later one queues behind, so a receipt that
// never comes, or a refusal about the nonce, sends the user back here.
func (u *user) resync(ctx context.Context) error {
	s, err := bench.NewSender(ctx, u.key, u.s.e.Eth, u.s.e.Submit, u.s.e.Rollup)
	if err != nil {
		return err
	}
	u.mu.Lock()
	u.sender = s
	u.mu.Unlock()
	return nil
}

// upkeep tops wallets up every fifteen minutes, as a user would buy more ZNN,
// and refreshes every sender's fee cap to the chain's current base fee so a
// fee market that moves over six hours does not strand a cap signed at the
// start.
func (s *sim) upkeep(ctx context.Context, p *PhaseRecord) {
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	type topUp struct {
		T       time.Time `json:"t"`
		Seconds float64   `json:"seconds"`
		Error   string    `json:"error,omitempty"`
	}
	var log []topUp
	for {
		select {
		case <-ctx.Done():
			p.set("topUps", log)
			return
		case <-t.C:
		}
		t0 := time.Now()
		var errs []string
		for _, pc := range s.o.Mix {
			if err := s.fund(ctx, p, pc, false); err != nil && ctx.Err() == nil {
				errs = append(errs, pc.Profile.Name+": "+err.Error())
			}
		}
		if h, err := s.e.Eth.HeaderByNumber(ctx, nil); err == nil {
			cap := s.e.Rollup.SuggestedFeeCap(h.BaseFee, rollup.DefaultTip())
			for _, u := range s.users {
				u.mu.Lock()
				if u.sender != nil {
					u.sender.FeeCap = cap
				}
				u.mu.Unlock()
			}
		}
		log = append(log, topUp{T: t0.UTC(), Seconds: time.Since(t0).Seconds(), Error: strings.Join(errs, "; ")})
	}
}

// --- the user's life -------------------------------------------------------------

func (u *user) think() time.Duration {
	mean := u.p.Think
	if pace := u.s.o.Pace; pace > 0 {
		mean = time.Duration(float64(mean) * pace)
	}
	d := time.Duration(u.rng.ExpFloat64() * float64(mean))
	return min(max(d, time.Second), 5*mean)
}

func (u *user) pick() string {
	names := make([]string, 0, len(u.p.Mix))
	total := 0.0
	for n, w := range u.p.Mix {
		names = append(names, n)
		total += w
	}
	sort.Strings(names)
	x := u.rng.Float64() * total
	for _, n := range names {
		if x -= u.p.Mix[n]; x < 0 {
			return n
		}
	}
	return names[len(names)-1]
}

func (u *user) live(ctx context.Context) {
	// A user's first action comes after a think, not the moment it arrives:
	// otherwise the ramp is a train of simultaneous first actions.
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(u.think()):
		}
		u.act(ctx, u.pick())
	}
}

// tx is what an action wants sent.
type txReq struct {
	to    *common.Address
	value *big.Int
	data  []byte
	gas   uint64 // zero estimates
	min   uint64 // floor on an estimate
	after func(r *types.Receipt)
	done  func() // runs whatever happens
	// native deposits are sends to the vault, not calls to the inbox
	deposit *derive.Call
}

func (u *user) act(ctx context.Context, action string) {
	rec := &Record{T: time.Now().UnixMilli(), User: u.id, Profile: u.p.Name, Action: action, Kind: "tx"}
	var req *txReq
	var err error
	switch action {
	case "browse-wallet", "browse-dex", "browse-nft":
		u.browse(ctx, rec)
		u.s.emit(rec)
		return
	case "nft-list":
		rec.Kind = "order"
		u.list(ctx, rec)
		u.s.emit(rec)
		return
	case "send-friend":
		req = &txReq{to: ptr(u.friend()), value: u.znn(0.01, 0.5), gas: 21_000}
	case "send-new":
		req = &txReq{to: ptr(bench.FreshAddress(u.addr, int(u.rng.Uint32()))), value: u.znn(0.01, 0.2), gas: 21_000}
	case "token-send":
		data, e := u.s.e.Arts.MustGet("BenchERC20").Call("transfer", u.friend(), wei(float64(1+u.rng.IntN(50))))
		req, err = &txReq{to: ptr(u.s.e.Dex.ERC20), value: new(big.Int), data: data}, e
	case "swap-buy":
		lo, hi := 0.02, 0.2
		if u.p.Name == "light" {
			lo, hi = 0.005, 0.05
		}
		req, err = u.swapBuy(u.znn(lo, hi))
	case "swap-sell":
		req, err = u.swapSell(ctx)
		if req == nil && err == nil {
			rec.Action = "swap-buy" // nothing to sell yet: buy instead, as a trader would
			req, err = u.swapBuy(u.znn(0.02, 0.2))
		}
	case "farm-deposit":
		data, e := u.s.e.Arts.MustGet("ZVMFarm").Call("deposit", big.NewInt(int64(u.s.pid)), wei(0.01), u.addr)
		req, err = &txReq{to: ptr(u.s.e.Apps.Farm), value: new(big.Int), data: data, min: 150_000}, e
	case "farm-harvest":
		data, e := u.s.e.Arts.MustGet("ZVMFarm").Call("harvest", big.NewInt(int64(u.s.pid)), u.addr)
		req, err = &txReq{to: ptr(u.s.e.Apps.Farm), value: new(big.Int), data: data, min: 150_000}, e
	case "nft-mint", "native-mint":
		data, e := u.s.e.Arts.MustGet("ZVMCollection").Call("publicMint", big.NewInt(1))
		req, err = &txReq{to: ptr(u.s.coll), value: new(big.Int), data: data, after: u.minted}, e
	case "nft-buy":
		req, err = u.buy(ctx, rec)
		if req == nil && err == nil {
			u.s.emit(rec)
			return
		}
	case "withdraw":
		to := devnet.Key(userWithdrawBase + uint32(u.id)).Address
		req = &txReq{to: ptr(rollup.BridgeAddr), value: wei(u.s.o.withdrawZNN()), data: bridge.EncodeWithdrawZNN(to), gas: 250_000,
			after: func(r *types.Receipt) { u.followWithdrawal(ctx, r) }}
	case "native-send":
		req = &txReq{to: ptr(u.friend()), value: u.znn(0.01, 0.2), gas: 21_000}
		if u.rng.IntN(5) == 0 {
			req.to = ptr(bench.FreshAddress(u.addr, int(u.rng.Uint32())))
		}
	case "native-deposit":
		self := u.addr
		amount := u.s.depositAmount()
		req = &txReq{deposit: &derive.Call{To: &self, Value: new(big.Int).Mul(amount, rollup.ZNNScale), Gas: 30_000}}
	default:
		err = fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		rec.Error = err.Error()
		u.s.emit(rec)
		return
	}
	u.send(ctx, rec, req)
}

func ptr[T any](v T) *T { return &v }

// depositAmount is one deposit in NoM's units, where ZNN has 8 decimals.
func (s *sim) depositAmount() *big.Int {
	f := new(big.Float).Mul(big.NewFloat(s.o.depositZNN()), new(big.Float).SetInt(znn.OneZNN))
	out, _ := f.Int(nil)
	return out
}

// znn is a random amount between lo and hi ZNN, in wei.
func (u *user) znn(lo, hi float64) *big.Int {
	return wei(math.Round((lo+u.rng.Float64()*(hi-lo))*1e4) / 1e4)
}

// friend is another simulated user: most value on a chain moves between
// people who are on it.
func (u *user) friend() common.Address {
	users := u.s.users
	for {
		v := users[u.rng.IntN(len(users))]
		if v != u || len(users) == 1 {
			return v.addr
		}
	}
}

// send publishes one transaction on the user's lane and waits for its
// receipt, holding one of the profile's pipeline slots while it does.
func (u *user) send(ctx context.Context, rec *Record, req *txReq) {
	select {
	case u.sem <- struct{}{}:
	case <-ctx.Done():
		if req.done != nil {
			req.done()
		}
		return
	}
	submitted := time.Now()
	rec.T = submitted.UnixMilli()
	var tx *types.Transaction
	var err error
	if u.acct != nil {
		tx, err = u.publishNative(ctx, req)
	} else {
		tx, err = u.publishBatch(ctx, req)
	}
	if err != nil {
		<-u.sem
		if req.done != nil {
			req.done()
		}
		if ctx.Err() != nil {
			return
		}
		rec.Error = err.Error()
		u.s.emit(rec)
		if u.key != nil && (strings.Contains(err.Error(), "nonce") || strings.Contains(err.Error(), "replacement")) {
			u.resync(u.s.followContext(ctx))
		}
		return
	}
	rec.Hash = tx.Hash().Hex()
	wait := func() {
		defer func() { <-u.sem }()
		if req.done != nil {
			defer req.done()
		}
		// The receipt is waited for past the run's end: a transaction the user
		// sent in its last minute is still part of what it saw.
		wctx := u.s.followContext(ctx)
		r, err := u.receipt(wctx, tx.Hash(), 10*time.Minute)
		rec.ConfirmMS = time.Since(submitted).Milliseconds()
		if err != nil {
			rec.Error = err.Error()
			rec.ConfirmMS = 0
			u.s.emit(rec)
			if u.key != nil {
				u.resync(wctx)
			}
			return
		}
		rec.Block, rec.GasUsed = r.BlockNumber.Uint64(), r.GasUsed
		if bt, ok := u.s.blockTimeOf(wctx, r.BlockNumber); ok {
			rec.IncludeMS = bt*1000 - submitted.UnixMilli()
		}
		rec.Reverted = r.Status != types.ReceiptStatusSuccessful
		rec.OK = !rec.Reverted
		if rec.OK && req.after != nil {
			req.after(r)
		}
		u.s.emit(rec)
	}
	if u.p.Pipeline > 1 {
		u.s.pending.Add(1)
		go func() { defer u.s.pending.Done(); wait() }()
	} else {
		wait()
	}
}

func (u *user) receipt(ctx context.Context, h common.Hash, timeout time.Duration) (*types.Receipt, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r, err := u.s.e.Eth.TransactionReceipt(ctx, h); err == nil {
			return r, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("no receipt within %s", timeout)
}

func (s *sim) blockTimeOf(ctx context.Context, n *big.Int) (int64, bool) {
	if v, ok := s.blockTime.Load(n.Uint64()); ok {
		return v.(int64), true
	}
	h, err := s.e.Eth.HeaderByNumber(ctx, n)
	if err != nil {
		return 0, false
	}
	s.blockTime.Store(n.Uint64(), int64(h.Time))
	return int64(h.Time), true
}

func (u *user) gasFor(ctx context.Context, req *txReq, limit uint64) (uint64, error) {
	if req.gas != 0 {
		return req.gas, nil
	}
	est, err := u.s.e.Eth.EstimateGas(ctx, ethereum.CallMsg{From: u.addr, To: req.to, Value: req.value, Data: req.data})
	if err != nil {
		return 0, fmt.Errorf("estimating gas: %w", err)
	}
	gas := max(est*13/10, req.min)
	if limit != 0 && gas > limit {
		gas = max(est, limit)
	}
	return gas, nil
}

func (u *user) publishBatch(ctx context.Context, req *txReq) (*types.Transaction, error) {
	gas, err := u.gasFor(ctx, req, u.s.e.Rollup.MaxTxGas)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.sender.Send(ctx, req.to, req.value, req.data, gas)
}

func (u *user) publishNative(ctx context.Context, req *txReq) (*types.Transaction, error) {
	e := u.s.e
	if req.deposit != nil {
		blk, err := u.acct.Send(ctx, e.Rollup.Vault, znn.ZNN, u.s.depositAmount(), derive.EncodeCall(*req.deposit))
		if err != nil {
			u.acct.Reset()
			return nil, err
		}
		return derive.DepositCallTx(e.Rollup, blk, *req.deposit), nil
	}
	gas, err := u.gasFor(ctx, req, e.Rollup.MaxNativeGas)
	if err != nil {
		return nil, err
	}
	_, tx, err := derive.PublishCall(ctx, e.Rollup, u.acct, znn.ZNN, derive.Call{To: req.to, Value: req.value, Gas: gas, Data: req.data})
	if err != nil {
		u.acct.Reset()
	}
	return tx, err
}

// --- swaps -------------------------------------------------------------------

func (u *user) swapBuy(value *big.Int) (*txReq, error) {
	d := u.s.e.Dex
	data, err := u.s.e.Arts.MustGet("UniswapV2Router02").Call("swapExactETHForTokens", new(big.Int),
		[]common.Address{d.WETH, d.ERC20}, u.addr, deadline())
	return &txReq{to: ptr(d.Router), value: value, data: data}, err
}

// swapSell sells part of what the user holds and is not already selling. It
// returns nil when there is nothing to sell.
func (u *user) swapSell(ctx context.Context) (*txReq, error) {
	e := u.s.e
	out, err := e.read(ctx, e.Dex.ERC20, "BenchERC20", "balanceOf", u.addr)
	if err != nil {
		return nil, err
	}
	bal := out[0].(*big.Int)
	micro := new(big.Int).Div(bal, big.NewInt(1e12)).Int64() // whole × 1e6
	free := micro - u.selling.Load()
	if free <= 1_000_000 { // less than one token
		return nil, nil
	}
	amount := free * int64(20+u.rng.IntN(41)) / 100
	u.selling.Add(amount)
	data, err := e.Arts.MustGet("UniswapV2Router02").Call("swapExactTokensForETH",
		new(big.Int).Mul(big.NewInt(amount), big.NewInt(1e12)), new(big.Int),
		[]common.Address{e.Dex.ERC20, e.Dex.WETH}, u.addr, deadline())
	if err != nil {
		u.selling.Add(-amount)
		return nil, err
	}
	return &txReq{to: ptr(e.Dex.Router), value: new(big.Int), data: data,
		done: func() { u.selling.Add(-amount) }}, nil
}

// --- NFTs --------------------------------------------------------------------

func (u *user) minted(r *types.Receipt) {
	for _, l := range r.Logs {
		if l.Address == u.s.coll && len(l.Topics) == 4 && l.Topics[0] == topicTransfer &&
			common.BytesToAddress(l.Topics[2].Bytes()) == u.addr {
			u.ownedMu.Lock()
			u.owned = append(u.owned, l.Topics[3].Big())
			u.ownedMu.Unlock()
		}
	}
}

// list signs a sell order for a token the user holds and posts it to the
// node's book — no transaction, one request (D28). A token sold since the
// user last looked is dropped from what it believes it holds, and it tries
// the next.
func (u *user) list(ctx context.Context, rec *Record) {
	e := u.s.e
	t0 := time.Now()
	defer func() { rec.LatencyMS = time.Since(t0).Milliseconds() }()
	for {
		u.ownedMu.Lock()
		if len(u.owned) == 0 {
			u.ownedMu.Unlock()
			rec.Skipped = "holds no token"
			return
		}
		i := u.rng.IntN(len(u.owned))
		id := u.owned[i]
		u.ownedMu.Unlock()
		out, err := e.read(ctx, u.s.coll, "ZVMCollection", "ownerOf", id)
		if err != nil || out[0].(common.Address) != u.addr {
			u.ownedMu.Lock()
			u.owned = slicesDelete(u.owned, id)
			u.ownedMu.Unlock()
			continue
		}
		now := time.Now()
		o := &orderbook.Order{
			Trader: u.addr, Side: orderbook.SideSell, Collection: u.s.coll, TokenID: orderbook.B(id),
			Amount: orderbook.U(1), Price: orderbook.B(u.znn(0.001, 0.02)),
			ListingTime: uint64(now.Add(-time.Minute).Unix()), ExpirationTime: uint64(now.Add(24 * time.Hour).Unix()),
			Nonce: orderbook.U(0), Salt: orderbook.B(new(big.Int).SetUint64(u.rng.Uint64())),
		}
		in, err := orderbook.Sign(u.key, u.s.domain, o)
		if err != nil {
			rec.Error = err.Error()
			return
		}
		t0 = time.Now()
		res, err := orderbook.Post(ctx, e.API, []*orderbook.Input{in})
		if err != nil {
			rec.Error = err.Error()
			return
		}
		if len(res) > 0 {
			rec.Hash = res[0].Hash.Hex()
		}
		rec.OK = true
		return
	}
}

func slicesDelete(ids []*big.Int, id *big.Int) []*big.Int {
	out := ids[:0]
	for _, x := range ids {
		if x.Cmp(id) != 0 {
			out = append(out, x)
		}
	}
	return out
}

// buy takes a fillable listing someone else posted, the way the collection
// page's "Buy now" does: read the book, re-read the order, send buy.
func (u *user) buy(ctx context.Context, rec *Record) (*txReq, error) {
	e := u.s.e
	var rows []struct {
		Hash   string `json:"hash"`
		Trader string `json:"trader"`
	}
	if err := e.getJSON(ctx, fmt.Sprintf("/api/nft/orders?collection=%s&side=sell&limit=200", u.s.coll.Hex()), &rows); err != nil {
		return nil, err
	}
	var theirs []string
	for _, r := range rows {
		if !strings.EqualFold(r.Trader, u.addr.Hex()) {
			theirs = append(theirs, r.Hash)
		}
	}
	u.rng.Shuffle(len(theirs), func(i, j int) { theirs[i], theirs[j] = theirs[j], theirs[i] })
	for _, h := range theirs[:min(len(theirs), 3)] {
		var one struct {
			Input    *orderbook.Input `json:"input"`
			Fillable bool             `json:"fillable"`
		}
		if err := e.getJSON(ctx, "/api/nft/orders/"+h, &one); err != nil || !one.Fillable || one.Input == nil {
			continue
		}
		data, err := e.Arts.MustGet("ZVMExchange").Call("buy", one.Input.ABI())
		if err != nil {
			return nil, err
		}
		id := one.Input.Order.TokenID.Big()
		return &txReq{to: ptr(e.Apps.Exchange), value: one.Input.Order.Price.Big(), data: data,
			after: func(*types.Receipt) {
				u.ownedMu.Lock()
				u.owned = append(u.owned, id)
				u.ownedMu.Unlock()
			}}, nil
	}
	rec.Skipped = "no fillable listing"
	return nil, nil
}

// --- the bridge ----------------------------------------------------------------

// followWithdrawal waits for the vault to pay a withdrawal out on NoM and
// records how long the user waited for their ZNN.
func (s *sim) followContext(fallback context.Context) context.Context {
	if s.followCtx != nil {
		return s.followCtx
	}
	return fallback
}

func (u *user) followWithdrawal(ctx context.Context, r *types.Receipt) {
	ctx = u.s.followContext(ctx)
	var id uint64
	for _, l := range r.Logs {
		if l.Address == rollup.BridgeAddr && len(l.Topics) == 4 && l.Topics[0] == bridge.TopicWithdrawalInitiated {
			id = l.Topics[1].Big().Uint64()
		}
	}
	if id == 0 {
		return
	}
	s := u.s
	bt, _ := s.blockTimeOf(ctx, r.BlockNumber)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		rec := &Record{T: bt * 1000, User: u.id, Profile: u.p.Name, Action: "withdraw-payout", Kind: "payout",
			Hash: r.TxHash.Hex()}
		deadline := time.Now().Add(90 * time.Minute)
		for time.Now().Before(deadline) {
			w, err := bridge.ReadWithdrawal(ctx, s.e.Eth, id)
			if err == nil && w.PaidIn != (common.Hash{}) {
				rec.OK = true
				rec.ConfirmMS = time.Now().UnixMilli() - rec.T
				s.emit(rec)
				return
			}
			select {
			case <-ctx.Done():
				rec.Error = "payout observation canceled"
				s.emit(rec)
				return
			case <-time.After(15 * time.Second):
			}
		}
		rec.Error = "not paid within 90 minutes of the withdrawal"
		s.emit(rec)
	}()
}

// --- page loads ------------------------------------------------------------------

// browse loads one explorer page's API calls at once, as the page does, and
// records the wall time of the slowest.
func (u *user) browse(ctx context.Context, rec *Record) {
	rec.Kind = "read"
	e := u.s.e
	a := u.addr.Hex()
	var paths []string
	switch rec.Action {
	case "browse-wallet":
		paths = []string{"/api/address/" + a, "/api/address/" + a + "/txs", "/api/address/" + a + "/transfers", "/api/prices"}
	case "browse-dex":
		pair := u.s.lp.Hex()
		paths = []string{"/api/dex/pairs", "/api/dex/pairs/" + pair + "/candles", "/api/dex/pairs/" + pair + "/trades"}
	case "browse-nft":
		c := u.s.coll.Hex()
		paths = []string{"/api/nft/collections", "/api/nft/collections/" + c + "/items", "/api/nft/listings", "/api/nft/sales"}
	}
	t0 := time.Now()
	var bytes atomic.Int64
	var bad atomic.Value
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.API+p, nil)
			if err != nil {
				bad.Store(err.Error())
				return
			}
			resp, err := e.http.Do(req)
			if err != nil {
				bad.Store(err.Error())
				return
			}
			n, _ := io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			bytes.Add(n)
			if resp.StatusCode >= 400 {
				bad.Store(fmt.Sprintf("%s: %s", p, resp.Status))
			}
		}()
	}
	wg.Wait()
	rec.LatencyMS = time.Since(t0).Milliseconds()
	rec.Bytes = bytes.Load()
	if v := bad.Load(); v != nil {
		rec.Error = v.(string)
		return
	}
	rec.OK = true
}
