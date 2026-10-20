package fleet

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Zenoglyphs-VM/zvm-bridge/bridge"
	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/devnet"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/Zenoglyphs-VM/zvm-node/bench"
	"github.com/Zenoglyphs-VM/zvm-node/media"
)

// Dist is a latency distribution in milliseconds.
type Dist struct {
	N    int     `json:"n"`
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
}

func dist(v []float64) Dist {
	if len(v) == 0 {
		return Dist{}
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	var sum float64
	for _, x := range s {
		sum += x
	}
	at := func(p float64) float64 { return s[int(float64(len(s)-1)*p)] }
	return Dist{N: len(s), Mean: sum / float64(len(s)), P50: at(0.5), P90: at(0.9), P99: at(0.99), Max: s[len(s)-1]}
}

// flow is one transaction a service phase follows to the chain and beyond.
type flow struct {
	Wallet    int    `json:"wallet"`
	Nom       string `json:"nom,omitempty"`
	Hash      string `json:"hash,omitempty"`
	SubmitMS  int64  `json:"submitMs"`
	Block     uint64 `json:"block,omitempty"`
	BlockTime int64  `json:"blockTime,omitempty"`
	Status    uint64 `json:"status"`
	Error     string `json:"error,omitempty"`
	ID        uint64 `json:"id,omitempty"`
	DoneMS    int64  `json:"doneMs,omitempty"`
	logs      []*types.Log
}

// track waits for each flow's receipt and stamps the block and its time.
func (e *Env) track(ctx context.Context, flows []*flow, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	times := map[uint64]int64{}
	var mu sync.Mutex
	for time.Now().Before(deadline) && ctx.Err() == nil {
		var pending []*flow
		for _, f := range flows {
			if f.Hash != "" && f.Block == 0 && f.Error == "" {
				pending = append(pending, f)
			}
		}
		if len(pending) == 0 {
			return
		}
		bench.Parallel(ctx, len(pending), 32, func(i int) error {
			f := pending[i]
			r, err := e.Eth.TransactionReceipt(ctx, common.HexToHash(f.Hash))
			if err != nil {
				return nil
			}
			mu.Lock()
			t, ok := times[r.BlockNumber.Uint64()]
			mu.Unlock()
			if !ok {
				h, err := e.Eth.HeaderByNumber(ctx, r.BlockNumber)
				if err != nil {
					return nil
				}
				t = int64(h.Time)
				mu.Lock()
				times[r.BlockNumber.Uint64()] = t
				mu.Unlock()
			}
			f.Block, f.BlockTime, f.Status, f.logs = r.BlockNumber.Uint64(), t, r.Status, r.Logs
			return nil
		})
		time.Sleep(3 * time.Second)
	}
}

type flowSummary struct {
	Submitted int            `json:"submitted"`
	Accepted  int            `json:"accepted"`
	Included  int            `json:"included"`
	Reverted  int            `json:"reverted"`
	Errors    map[string]int `json:"errors,omitempty"`
	Inclusion Dist           `json:"inclusionMs"` // submit → the block's timestamp
	Done      Dist           `json:"doneMs"`      // submit → the phase's own end condition
	Blocks    int            `json:"blocks"`
}

func summarize(flows []*flow) flowSummary {
	s := flowSummary{Submitted: len(flows), Errors: map[string]int{}}
	var inc, done []float64
	blocks := map[uint64]bool{}
	for _, f := range flows {
		if f.Error != "" {
			s.Errors[errorKind(f.Error)]++
			continue
		}
		s.Accepted++
		if f.Block != 0 {
			s.Included++
			blocks[f.Block] = true
			if f.Status == 0 {
				s.Reverted++
			}
			inc = append(inc, float64(f.BlockTime*1000-f.SubmitMS))
		}
		if f.DoneMS != 0 {
			done = append(done, float64(f.DoneMS-f.SubmitMS))
		}
	}
	s.Inclusion, s.Done, s.Blocks = dist(inc), dist(done), len(blocks)
	return s
}

// --- native onboarding: fusion -------------------------------------------------

var plasmaContract = znn.MustParseAddress("z1qxemdeddedxplasmaxxxxxxxxxxxxxxxxsctrp")

func fuseData(beneficiary znn.Address) []byte {
	sel := znn.Sha3([]byte("Fuse(address)"))
	return append(append([]byte{}, sel[:4]...), common.LeftPadBytes(beneficiary[:], 32)...)
}

// onboardNative fuses QSR for count new Zenon accounts, from two funded L1
// accounts at once, and times how long NoM takes to make the plasma usable.
// This is what a Syrius user needs before the native lane will carry anything.
func (e *Env) onboardNative(ctx context.Context, p *PhaseRecord, count int, fuseQSR int64) error {
	return e.onboardNativeAt(ctx, p, nativeBase, count, fuseQSR)
}

// onboardNativeAt fuses for count accounts from base. The user simulation
// draws its Zenon keys from its own range and has to fuse them the same way,
// and on a shared devnet only some of a range is already fused.
func (e *Env) onboardNativeAt(ctx context.Context, p *PhaseRecord, base uint32, count int, fuseQSR int64) error {
	addrs := make([]znn.Address, count)
	fused := make([]bool, count)
	for i := range addrs {
		addrs[i] = devnet.Key(base + uint32(i)).Address
	}
	if err := bench.Parallel(ctx, count, 32, func(i int) error {
		info, err := e.L1.PlasmaInfo(ctx, addrs[i])
		if err != nil {
			return err
		}
		fused[i] = info.MaxPlasma > 0
		return nil
	}); err != nil {
		return err
	}
	var todo []int
	for i, f := range fused {
		if !f {
			todo = append(todo, i)
		}
	}
	p.set("alreadyFused", count-len(todo))
	e.Log.Info("fusing for new native wallets", "new", len(todo), "qsrEach", fuseQSR)

	type fuse struct {
		Index     int    `json:"index"`
		Funder    uint32 `json:"funder"`
		SubmitMS  int64  `json:"submitMs"`
		PublishMS int64  `json:"publishedMs"`
		Attempts  int    `json:"attempts"`
		VisibleMS int64  `json:"visibleMs,omitempty"`
		Error     string `json:"error,omitempty"`
	}
	funders, err := e.fundersHolding(ctx, znn.QSR, devnet.Units(fuseQSR*4))
	if err != nil {
		return err
	}
	fuses := make([]*fuse, len(todo))
	var emu sync.Mutex
	refusals := map[string]int{}
	start := time.Now()
	var wg sync.WaitGroup
	for fi, funder := range funders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			acct := znn.NewAccount(devnet.Key(funder), e.L1, e.Rollup.L1ChainID)
			for j := fi; j < len(todo); j += len(funders) {
				f := &fuse{Index: todo[j], Funder: funder, SubmitMS: time.Now().UnixMilli()}
				fuses[j] = f
				for f.Attempts < 120 && ctx.Err() == nil {
					f.Attempts++
					_, err := acct.Send(ctx, plasmaContract, znn.QSR, devnet.Units(fuseQSR), fuseData(addrs[todo[j]]))
					if err == nil {
						f.PublishMS, f.Error = time.Now().UnixMilli(), ""
						break
					}
					f.Error = err.Error()
					emu.Lock()
					refusals[errorKind(err.Error())]++
					emu.Unlock()
					time.Sleep(5 * time.Second)
				}
			}
		}()
	}
	wg.Wait()
	published := time.Now()
	p.set("publishSeconds", published.Sub(start).Seconds())
	p.set("refusals", refusals)

	// When is the plasma there? Poll until every account has it.
	type point struct {
		T     int64 `json:"t"`
		Fused int   `json:"fused"`
	}
	var series []point
	visible := map[int]int64{}
	for time.Since(published) < 20*time.Minute && ctx.Err() == nil {
		var mu sync.Mutex
		bench.Parallel(ctx, len(fuses), 32, func(i int) error {
			f := fuses[i]
			if f == nil || f.PublishMS == 0 {
				return nil
			}
			mu.Lock()
			_, seen := visible[f.Index]
			mu.Unlock()
			if seen {
				return nil
			}
			if info, err := e.L1.PlasmaInfo(ctx, addrs[f.Index]); err == nil && info.MaxPlasma > 0 {
				mu.Lock()
				visible[f.Index] = time.Now().UnixMilli()
				f.VisibleMS = visible[f.Index]
				mu.Unlock()
			}
			return nil
		})
		series = append(series, point{time.Now().UnixMilli(), len(visible)})
		if len(visible) >= countPublished(fuses) {
			break
		}
		time.Sleep(10 * time.Second)
	}
	var pub, vis []float64
	for _, f := range fuses {
		if f != nil && f.PublishMS != 0 {
			pub = append(pub, float64(f.PublishMS-f.SubmitMS))
			if f.VisibleMS != 0 {
				vis = append(vis, float64(f.VisibleMS-f.SubmitMS))
			}
		}
	}
	p.set("fuses", fuses)
	p.set("visibleSeries", series)
	p.set("publishMs", dist(pub))
	p.set("visibleMs", dist(vis))
	p.set("visible", len(visible))
	p.set("allVisibleSeconds", time.Since(start).Seconds())
	if e.Rollup.Fee != nil && e.Rollup.Fee.FromHeight != 0 {
		if err := e.fundNativeZNNAt(ctx, p, addrs, base, devnet.Units(1)); err != nil {
			return err
		}
	}
	if len(visible)+count-len(todo) >= count {
		e.state.Onboarded = max(e.state.Onboarded, count)
		return nil
	}
	return fmt.Errorf("%d of %d new accounts have plasma", len(visible), len(todo))
}

func countPublished[T interface{ comparable }](fuses []*T) int {
	n := 0
	for _, f := range fuses {
		if f != nil {
			n++
		}
	}
	return n
}

// fundersHolding is the L1 funding accounts that can actually pay: on a shared
// devnet the benchmark funder holds what is left of it, not a genesis balance,
// and a funder with nothing to send does not fail — it is refused and retried
// for ten minutes while the other one does all the work.
func (e *Env) fundersHolding(ctx context.Context, token znn.TokenStandard, least *big.Int) ([]uint32, error) {
	var out []uint32
	for _, idx := range []uint32{devnet.DevOwner, devnet.BenchFunder} {
		info, err := e.L1.AccountInfo(ctx, devnet.Key(idx).Address)
		if err != nil {
			return nil, err
		}
		if info.Balance(token).Cmp(least) >= 0 {
			out = append(out, idx)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no L1 funding account holds %s of %s", least, token)
	}
	return out, nil
}

// --- the bridge ------------------------------------------------------------------

// bridgeDeposits sends deposits from the genesis native wallets, each a vault
// send carrying a credit to the depositor's own 20 bytes, and times the credit
// on the ZVM and the vault's receives on NoM.
func (e *Env) bridgeDeposits(ctx context.Context, p *PhaseRecord, total int) error {
	wallets := min(devnet.NumBenchNative, total)
	per := (total + wallets - 1) / wallets
	flows := make([][]*flow, wallets)
	start := time.Now()
	bench.Parallel(ctx, wallets, wallets, func(i int) error {
		key := devnet.Key(devnet.BenchNative + uint32(i))
		acct := znn.NewAccount(key, e.L1, e.Rollup.L1ChainID)
		acct.AssumeFused = true
		self := common.Address(key.Address)
		for j := 0; j < per; j++ {
			call := derive.Call{To: &self, Value: new(big.Int).Mul(znn.OneZNN, rollup.ZNNScale), Gas: 30_000}
			f := &flow{Wallet: i, SubmitMS: time.Now().UnixMilli()}
			blk, err := acct.Send(ctx, e.Rollup.Vault, znn.ZNN, znn.OneZNN, derive.EncodeCall(call))
			if err != nil {
				f.Error = err.Error()
			} else {
				f.Nom = blk.Hash.String()
				f.Hash = derive.DepositCallTx(e.Rollup, blk, call).Hash().Hex()
			}
			flows[i] = append(flows[i], f)
		}
		return nil
	})
	var all []*flow
	for _, fs := range flows {
		all = append(all, fs...)
	}
	p.set("publishSeconds", time.Since(start).Seconds())
	e.track(ctx, all, 15*time.Minute)
	p.set("creditedSeconds", time.Since(start).Seconds())

	type point struct {
		T          int64 `json:"t"`
		Unreceived int64 `json:"unreceived"`
	}
	var series []point
	for time.Since(start) < 30*time.Minute && ctx.Err() == nil {
		list, err := e.L1.UnreceivedBlocks(ctx, e.Rollup.Vault, 0, 1)
		if err == nil {
			series = append(series, point{time.Now().UnixMilli(), int64(list.Count)})
			if list.Count == 0 {
				break
			}
		}
		time.Sleep(5 * time.Second)
	}
	p.set("vaultUnreceived", series)
	p.set("vaultDrainedSeconds", time.Since(start).Seconds())
	p.set("deposits", summarize(all))
	p.set("flows", all)
	return nil
}

// bridgeWithdrawals withdraws 1 ZNN from each of n EVM wallets and follows
// every withdrawal to the payout the vault makes on NoM and back into the
// bridge contract.
func (e *Env) bridgeWithdrawals(ctx context.Context, p *PhaseRecord, n int) error {
	if _, err := e.run(ctx, p, bench.RunConfig{Scenario: "fund", Custom: noop, Lane: "batch", Wallets: n,
		WalletBase: scaleBase, Fund: wei(4), MinBalance: wei(2), Duration: time.Nanosecond, Drain: time.Second}); err != nil {
		return err
	}
	flows := make([]*flow, n)
	start := time.Now()
	bench.Parallel(ctx, n, 64, func(i int) error {
		f := &flow{Wallet: i}
		flows[i] = f
		s, err := bench.NewSender(ctx, devnet.EVMKey(scaleBase+uint32(i)), e.Eth, e.Submit, e.Rollup)
		if err != nil {
			f.Error = err.Error()
			return nil
		}
		to := devnet.Key(withdrawBase + uint32(i)).Address
		bridgeAddr := rollup.BridgeAddr
		f.SubmitMS = time.Now().UnixMilli()
		tx, err := s.Send(ctx, &bridgeAddr, wei(1), bridge.EncodeWithdrawZNN(to), 250_000)
		if err != nil {
			f.Error = err.Error()
		} else {
			f.Hash = tx.Hash().Hex()
		}
		return nil
	})
	p.set("submitSeconds", time.Since(start).Seconds())
	e.track(ctx, flows, 15*time.Minute)
	for _, f := range flows {
		for _, l := range f.logs {
			if l.Address == rollup.BridgeAddr && len(l.Topics) == 4 && l.Topics[0] == bridge.TopicWithdrawalInitiated {
				f.ID = l.Topics[1].Big().Uint64()
			}
		}
	}

	type point struct {
		T    int64 `json:"t"`
		Paid int   `json:"paid"`
	}
	var series []point
	for time.Since(start) < 45*time.Minute && ctx.Err() == nil {
		var pending []*flow
		paid := 0
		for _, f := range flows {
			if f.ID != 0 {
				if f.DoneMS == 0 {
					pending = append(pending, f)
				} else {
					paid++
				}
			}
		}
		series = append(series, point{time.Now().UnixMilli(), paid})
		if len(pending) == 0 {
			break
		}
		bench.Parallel(ctx, len(pending), 32, func(i int) error {
			w, err := bridge.ReadWithdrawal(ctx, e.Eth, pending[i].ID)
			if err == nil && w.PaidIn != (common.Hash{}) {
				pending[i].DoneMS = time.Now().UnixMilli()
			}
			return nil
		})
		time.Sleep(10 * time.Second)
	}
	p.set("paidSeries", series)
	p.set("withdrawals", summarize(flows))
	p.set("flows", flows)
	return nil
}

// --- the faucet ----------------------------------------------------------------

type dripRecord struct {
	Index     int    `json:"index"`
	Address   string `json:"address"`
	Client    string `json:"client"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latencyMs"`
	L2Tx      string `json:"l2Tx,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (e *Env) drip(ctx context.Context, i int, address, client string) *dripRecord {
	d := &dripRecord{Index: i, Address: address, Client: client}
	body, _ := json.Marshal(map[string]string{"address": address})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.API+"/api/faucet", bytes.NewReader(body))
	if err != nil {
		d.Error = err.Error()
		return d
	}
	req.Header.Set("Content-Type", "application/json")
	if client != "" {
		req.Header.Set("X-Forwarded-For", client)
	}
	t0 := time.Now()
	resp, err := e.http.Do(req)
	d.LatencyMS = time.Since(t0).Milliseconds()
	if err != nil {
		d.Error = err.Error()
		return d
	}
	defer resp.Body.Close()
	d.Status = resp.StatusCode
	var out map[string]any
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if s, ok := out["l2Tx"].(string); ok {
		d.L2Tx = s
	}
	if s, ok := out["error"].(string); ok {
		d.Error = s
	}
	return d
}

// faucetBurst asks the faucet for n fresh addresses, the way the /faucet page
// does, through the explorer's proxy. Every request names a different client
// in X-Forwarded-For: the faucet keys its cooldown on the first hop of that
// header, so this is also the check of whether the cooldown can be walked
// around.
func (e *Env) faucetBurst(ctx context.Context, p *PhaseRecord, n, conc int) error {
	drips := make([]*dripRecord, n)
	start := time.Now()
	bench.Parallel(ctx, n, conc, func(i int) error {
		addr := devnet.Key(faucetBase + uint32(i)).Address.String()
		drips[i] = e.drip(ctx, i, addr, fmt.Sprintf("198.18.%d.%d", i/250, i%250+1))
		return nil
	})
	took := time.Since(start)
	served := 0
	var lat []float64
	flows := []*flow{}
	statuses := map[int]int{}
	errs := map[string]int{}
	for _, d := range drips {
		statuses[d.Status]++
		if d.Status == http.StatusOK {
			served++
			lat = append(lat, float64(d.LatencyMS))
			flows = append(flows, &flow{Wallet: d.Index, Hash: d.L2Tx, SubmitMS: start.UnixMilli()})
		} else if d.Error != "" {
			errs[errorKind(d.Error)]++
		}
	}
	p.set("seconds", took.Seconds())
	p.set("served", served)
	p.set("perMinute", float64(served)/took.Minutes())
	p.set("responseMs", dist(lat))
	p.set("statuses", statuses)
	p.set("errors", errs)
	p.set("drips", drips)

	probes := []struct {
		Name    string `json:"name"`
		Address string `json:"address"`
		Client  string `json:"client"`
	}{
		{"an address already served, under a new forwarded client", drips[0].Address, "198.18.200.1"},
		{"a new address with no forwarded header (the real client)", devnet.Key(faucetBase + uint32(n)).Address.String(), ""},
		{"another new address from the same real client", devnet.Key(faucetBase + uint32(n+1)).Address.String(), ""},
		{"a third new address, the same real client, a forged header", devnet.Key(faucetBase + uint32(n+2)).Address.String(), "203.0.113.77"},
	}
	var results []map[string]any
	for i, pr := range probes {
		d := e.drip(ctx, n+i, pr.Address, pr.Client)
		results = append(results, map[string]any{"probe": pr.Name, "status": d.Status, "error": d.Error})
	}
	p.set("cooldownProbes", results)

	e.track(ctx, flows, 10*time.Minute)
	p.set("l2", summarize(flows))
	return nil
}

// --- the media store -----------------------------------------------------------

type mediaSession struct {
	addr  common.Address
	token string
}

func (e *Env) openSession(ctx context.Context, index uint32) (*mediaSession, error) {
	key := devnet.EVMKey(index)
	addr := crypto.PubkeyToAddress(key.PublicKey)
	var ch struct {
		Message string `json:"message"`
	}
	if err := e.getJSON(ctx, "/api/media/challenge?address="+addr.Hex(), &ch); err != nil {
		return nil, err
	}
	sig, err := crypto.Sign(accounts.TextHash([]byte(ch.Message)), key)
	if err != nil {
		return nil, err
	}
	sig[64] += 27
	body, _ := json.Marshal(media.SessionRequest{Address: addr.Hex(), Message: ch.Message, Signature: hex.EncodeToString(sig)})
	resp, err := e.http.Post(e.API+"/api/media/session", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Token == "" {
		return nil, fmt.Errorf("session: %s %s", resp.Status, out.Error)
	}
	return &mediaSession{addr: addr, token: out.Token}, nil
}

func (e *Env) upload(ctx context.Context, s *mediaSession, data []byte, contentType string) (int, string, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.API+"/api/media", bytes.NewReader(data))
	if err != nil {
		return 0, "", 0, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+s.token)
	t0 := time.Now()
	resp, err := e.http.Do(req)
	if err != nil {
		return 0, "", time.Since(t0), err
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	took := time.Since(t0)
	hash, _ := out["hash"].(string)
	if msg, ok := out["error"].(string); ok && resp.StatusCode >= 300 {
		return resp.StatusCode, hash, took, errors.New(msg)
	}
	return resp.StatusCode, hash, took, nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func fakePNG(size int) []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), randomBytes(max(0, size-8))...)
}

func fakeJSON(size int) []byte {
	pad := hex.EncodeToString(randomBytes(max(1, (size-40)/2)))
	raw, _ := json.Marshal(map[string]string{"name": "ZVM fleet object", "pad": pad})
	return raw
}

// mediaLoad opens signed sessions for many accounts and uploads a mix of
// metadata and images up to the size limit, then reads one object back under
// load. It also checks the limits refuse what they should.
func (e *Env) mediaLoad(ctx context.Context, p *PhaseRecord, uploads int) error {
	const sessions = 100
	ss := make([]*mediaSession, sessions)
	var slat []float64
	var smu sync.Mutex
	if err := bench.Parallel(ctx, sessions, 16, func(i int) error {
		t0 := time.Now()
		s, err := e.openSession(ctx, mediaBase+uint32(i))
		if err != nil {
			return err
		}
		smu.Lock()
		slat = append(slat, float64(time.Since(t0).Milliseconds()))
		smu.Unlock()
		ss[i] = s
		return nil
	}); err != nil {
		return err
	}
	p.set("sessionMs", dist(slat))

	type kind struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		Size  int    `json:"size"`
		Count int    `json:"count"`
	}
	kinds := []kind{
		{"json 2 KB", "application/json", 2 << 10, uploads * 40 / 100},
		{"png 1 KB", "image/png", 1 << 10, uploads * 40 / 100},
		{"png 256 KB", "image/png", 256 << 10, uploads * 12 / 100},
		{"png 2 MB", "image/png", 2 << 20, uploads * 7 / 100},
		{"png 9.9 MB", "image/png", 10<<20 - 100_000, max(4, uploads/100)},
	}
	type job struct{ k int }
	var jobs []job
	for ki, k := range kinds {
		for i := 0; i < k.Count; i++ {
			jobs = append(jobs, job{ki})
		}
	}
	type kindResult struct {
		Kind    kind           `json:"kind"`
		OK      int            `json:"ok"`
		Errors  map[string]int `json:"errors,omitempty"`
		Latency Dist           `json:"latencyMs"`
		Bytes   int64          `json:"bytes"`
		lat     []float64
	}
	results := make([]*kindResult, len(kinds))
	for i, k := range kinds {
		results[i] = &kindResult{Kind: k, Errors: map[string]int{}}
	}
	var rmu sync.Mutex
	var sample string
	start := time.Now()
	bench.Parallel(ctx, len(jobs), 16, func(i int) error {
		k := kinds[jobs[i].k]
		var data []byte
		if k.Type == "application/json" {
			data = fakeJSON(k.Size)
		} else {
			data = fakePNG(k.Size)
		}
		status, hash, took, err := e.upload(ctx, ss[i%sessions], data, k.Type)
		rmu.Lock()
		defer rmu.Unlock()
		r := results[jobs[i].k]
		if err != nil || status != http.StatusCreated {
			msg := fmt.Sprintf("status %d", status)
			if err != nil {
				msg = errorKind(err.Error())
			}
			r.Errors[msg]++
			return nil
		}
		r.OK++
		r.Bytes += int64(len(data))
		r.lat = append(r.lat, float64(took.Milliseconds()))
		if k.Size == 256<<10 && sample == "" {
			sample = hash
		}
		return nil
	})
	var totalBytes int64
	var totalOK int
	for _, r := range results {
		r.Latency = dist(r.lat)
		totalBytes += r.Bytes
		totalOK += r.OK
	}
	took := time.Since(start)
	p.set("uploads", results)
	p.set("uploadSeconds", took.Seconds())
	p.set("uploadedBytes", totalBytes)
	p.set("uploadedObjects", totalOK)
	p.set("uploadMBps", float64(totalBytes)/1e6/took.Seconds())

	// Limits: one byte over each cap, and a duplicate.
	dup := fakePNG(4096)
	checks := []struct {
		name string
		data []byte
		ct   string
	}{
		{"image one byte over 10 MB", fakePNG(10<<20 + 1), "image/png"},
		{"JSON one byte over 256 KB", fakeJSON(256<<10 + 64), "application/json"},
		{"not an image", randomBytes(2048), "image/png"},
		{"the same object twice (first)", dup, "image/png"},
		{"the same object twice (second)", dup, "image/png"},
	}
	var limitResults []map[string]any
	for _, c := range checks {
		status, hash, _, err := e.upload(ctx, ss[0], c.data, c.ct)
		r := map[string]any{"check": c.name, "status": status, "bytes": len(c.data), "hash": hash}
		if err != nil {
			r["error"] = err.Error()
		}
		limitResults = append(limitResults, r)
	}
	p.set("limits", limitResults)

	// Reads: one 256 KB object, many times, as a gallery would.
	if sample != "" {
		ep := Endpoint{Name: "media object 256 KB", Method: http.MethodGet, Path: "/media/" + sample + ".png"}
		p.set("serve", e.load(ctx, []Endpoint{ep}, 32, 8*time.Second))
	}
	return nil
}

// --- contract verification -----------------------------------------------------

var importRE = regexp.MustCompile(`import\s+(?:[^;"']*?\s+from\s+)?["']([^"']+)["']`)

// standardInput is the solc standard JSON the verify page takes, built from the
// in-tree sources exactly as `npm run verify:contracts` builds it.
func standardInput(contracts, root string) (json.RawMessage, error) {
	sources := map[string]map[string]string{}
	todo := []string{root}
	for len(todo) > 0 {
		unit := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if _, ok := sources[unit]; ok {
			continue
		}
		file := filepath.Join(contracts, unit)
		if strings.HasPrefix(unit, "@") {
			file = filepath.Join(contracts, "node_modules", unit)
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		sources[unit] = map[string]string{"content": string(raw)}
		for _, m := range importRE.FindAllStringSubmatch(string(raw), -1) {
			imp := m[1]
			if !strings.HasPrefix(imp, ".") {
				todo = append(todo, imp)
				continue
			}
			parts := strings.Split(unit, "/")
			parts = parts[:len(parts)-1]
			for _, seg := range strings.Split(imp, "/") {
				switch seg {
				case "..":
					parts = parts[:len(parts)-1]
				case ".":
				default:
					parts = append(parts, seg)
				}
			}
			todo = append(todo, strings.Join(parts, "/"))
		}
	}
	return json.Marshal(map[string]any{
		"language": "Solidity",
		"sources":  sources,
		"settings": map[string]any{"optimizer": map[string]any{"enabled": true, "runs": 200}, "evmVersion": "prague"},
	})
}

// verifyLoad deploys n copies of the benchmark ERC-20 and submits all of them
// for verification at once.
func (e *Env) verifyLoad(ctx context.Context, p *PhaseRecord, n int) error {
	input, err := standardInput(filepath.Join(e.Repo, "contracts"), "bench/BenchTokens.sol")
	if err != nil {
		return err
	}
	p.set("inputBytes", len(input))
	var compilers struct {
		Builds []struct {
			Version     string `json:"version"`
			LongVersion string `json:"longVersion"`
		} `json:"builds"`
	}
	if err := e.getJSON(ctx, "/api/verify/compilers", &compilers); err != nil {
		return err
	}
	compiler := ""
	for _, b := range compilers.Builds {
		if b.Version == "0.8.30" {
			compiler = b.LongVersion
		}
	}
	if compiler == "" {
		return errors.New("solc 0.8.30 is not in the compiler list")
	}

	s, err := e.funder(ctx)
	if err != nil {
		return err
	}
	art := e.Arts.MustGet("BenchERC20")
	var txs []*types.Transaction
	for i := 0; i < n; i++ {
		init, err := art.Deploy(fmt.Sprintf("Fleet Verify %d", time.Now().UnixNano()%1_000_000+int64(i)), "FV", big.NewInt(1_000_000))
		if err != nil {
			return err
		}
		tx, err := s.Send(ctx, nil, new(big.Int), init, 1_400_000)
		if err != nil {
			return err
		}
		txs = append(txs, tx)
	}
	addrs := make([]common.Address, n)
	for i, tx := range txs {
		r, err := bench.WaitReceipt(ctx, e.Eth, tx, 5*time.Minute)
		if err != nil {
			return err
		}
		addrs[i] = r.ContractAddress
	}

	type job struct {
		Address  string `json:"address"`
		ID       string `json:"id"`
		Status   string `json:"status"`
		Message  string `json:"message,omitempty"`
		Match    any    `json:"match,omitempty"`
		SubmitMS int64  `json:"submitMs"`
		AcceptMS int64  `json:"acceptMs"`
		DoneMS   int64  `json:"doneMs,omitempty"`
	}
	jobs := make([]*job, n)
	bench.Parallel(ctx, n, n, func(i int) error {
		j := &job{Address: addrs[i].Hex(), SubmitMS: time.Now().UnixMilli()}
		jobs[i] = j
		body, _ := json.Marshal(map[string]any{"address": j.Address, "compiler": compiler,
			"contract": "bench/BenchTokens.sol:BenchERC20", "input": json.RawMessage(input)})
		resp, err := e.http.Post(e.API+"/api/verify", "application/json", bytes.NewReader(body))
		if err != nil {
			j.Status, j.Message = "error", err.Error()
			return nil
		}
		defer resp.Body.Close()
		j.AcceptMS = time.Now().UnixMilli()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		j.ID, _ = out["id"].(string)
		j.Status, _ = out["status"].(string)
		if msg, ok := out["error"].(string); ok {
			j.Status, j.Message = "refused", msg
		}
		return nil
	})
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		open := 0
		for _, j := range jobs {
			if j.ID == "" || j.DoneMS != 0 {
				continue
			}
			var out map[string]any
			if e.getJSON(ctx, "/api/verify/"+j.ID, &out) != nil {
				open++
				continue
			}
			j.Status, _ = out["status"].(string)
			j.Message, _ = out["message"].(string)
			j.Match = out["match"]
			if j.Status == "verified" || j.Status == "failed" {
				j.DoneMS = time.Now().UnixMilli()
			} else {
				open++
			}
		}
		if open == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	var done []float64
	for _, j := range jobs {
		if j.DoneMS != 0 {
			done = append(done, float64(j.DoneMS-j.SubmitMS))
		}
	}
	p.set("compiler", compiler)
	p.set("jobs", jobs)
	p.set("doneMs", dist(done))
	return nil
}

// --- the second node -----------------------------------------------------------

// followerCheck compares the two nodes at the follower's head, then restarts
// the follower's execution client — which flushes its in-memory state to disk
// — and times its recovery. The restart is the only way to see what the state
// costs on disk, because both clients hold recent state in memory. Every call
// below names the container found running: the restart used to name
// zvm-op-geth-b outright, which has not existed since op-reth replaced it (D2).
func (e *Env) followerCheck(ctx context.Context, p *PhaseRecord) error {
	engine := ""
	for _, c := range e.Names.EngineBCandidates {
		if e.running[c] {
			engine = c
			break
		}
	}
	if e.Follower == nil || engine == "" {
		return errors.New("no second node running (docker compose --profile verify up -d)")
	}
	hb, err := e.Follower.BlockNumber(ctx)
	if err != nil {
		return err
	}
	a, err := e.Eth.HeaderByNumber(ctx, new(big.Int).SetUint64(hb))
	if err != nil {
		return err
	}
	b, err := e.Follower.HeaderByNumber(ctx, new(big.Int).SetUint64(hb))
	if err != nil {
		return err
	}
	p.set("agreement", map[string]any{"block": hb, "sameHash": a.Hash() == b.Hash(), "sameStateRoot": a.Root == b.Root,
		"hash": a.Hash().Hex(), "stateRoot": a.Root.Hex()})

	before := e.dirSizes(ctx, engine)
	t0 := time.Now()
	if out, err := exec.CommandContext(ctx, "docker", "restart", "-t", "300", engine).CombinedOutput(); err != nil {
		return fmt.Errorf("restarting %s: %v: %s", engine, err, out)
	}
	restarted := time.Since(t0)
	after := e.dirSizes(ctx, engine)
	var caughtUp time.Duration
	for time.Since(t0) < 15*time.Minute && ctx.Err() == nil {
		fh, err1 := e.Follower.BlockNumber(ctx)
		ph, err2 := e.Eth.BlockNumber(ctx)
		if err1 == nil && err2 == nil && fh+1 >= ph {
			caughtUp = time.Since(t0)
			break
		}
		time.Sleep(2 * time.Second)
	}
	p.set("restart", map[string]any{"stopStartSeconds": restarted.Seconds(), "caughtUpSeconds": caughtUp.Seconds(),
		"dirsBefore": before, "dirsAfter": after})
	return nil
}

var _ = fs.ErrNotExist

// fundNative gives each native wallet ZNN to burn, on a chain that prices
// native calls (D9). Fusion buys plasma, not gas: since the burned fee was
// switched on, a native call's gas is also bounded by what it burns, so a
// wallet holding no ZNN cannot publish one at all. Measured 2026-09-17 on
// chain 70: every native transfer and mint failed "insufficient balance for
// transfer". One ZNN is ~330 transfers or ~28 calls of 250 k gas at the quote
// that day, well past what a phase sends from one wallet.
func (e *Env) fundNative(ctx context.Context, p *PhaseRecord, addrs []znn.Address) error {
	if e.Rollup.Fee == nil || e.Rollup.Fee.FromHeight == 0 {
		return nil
	}
	return e.fundNativeZNNAt(ctx, p, addrs, nativeBase, devnet.Units(1))
}

// fundNativeZNNAt tops each wallet up to want ZNN on NoM when it holds less
// than half of it. addrs[i] must be devnet.Key(base+i): the wallets receive
// their own sends, so the key is derived from the index.
func (e *Env) fundNativeZNNAt(ctx context.Context, p *PhaseRecord, addrs []znn.Address, base uint32, want *big.Int) error {
	low := new(big.Int).Div(want, big.NewInt(2))
	short := make([]bool, len(addrs))
	pending := make([]bool, len(addrs))
	if err := bench.Parallel(ctx, len(addrs), 32, func(i int) error {
		info, err := e.L1.AccountInfo(ctx, addrs[i])
		if err != nil {
			return err
		}
		if info.Balance(znn.ZNN).Cmp(low) >= 0 {
			return nil
		}
		short[i] = true
		un, err := e.L1.UnreceivedBlocks(ctx, addrs[i], 0, 10)
		if err != nil {
			return err
		}
		for _, b := range un.List {
			if b.TokenStandard == znn.ZNN {
				pending[i] = true
			}
		}
		return nil
	}); err != nil {
		return err
	}
	var send []int
	var need []int
	for i := range addrs {
		if short[i] {
			need = append(need, i)
			if !pending[i] {
				send = append(send, i)
			}
		}
	}
	p.set("nativeFunding", map[string]any{"short": len(need), "sent": len(send), "znnEach": new(big.Int).Div(want, znn.OneZNN).Int64()})
	if len(need) == 0 {
		return nil
	}
	e.Log.Info("funding native wallets with ZNN to burn", "wallets", len(need), "sending", len(send))
	// A shared queue rather than a share each, retried round by round: on a
	// devnet nobody genesised this week, one funder holds thousands of ZNN and
	// the other holds what is left of a hundred. Splitting the work evenly made
	// the poorer one fail its half — measured on chain 69 on 2026-09-22, where
	// the benchmark funder ran out five wallets in and the preparation stopped.
	// A spent funder now retires and what it did not send is picked up by the
	// others.
	unsent := append([]int(nil), send...)
	for round := 0; len(unsent) > 0 && round < 4 && ctx.Err() == nil; round++ {
		funders, err := e.fundersHolding(ctx, znn.ZNN, want)
		if err != nil {
			return fmt.Errorf("%w — %d wallets still unfunded", err, len(unsent))
		}
		queue := make(chan int, len(unsent))
		for _, i := range unsent {
			queue <- i
		}
		close(queue)
		var (
			mu     sync.Mutex
			failed []int
			wg     sync.WaitGroup
		)
		errs := make([]error, len(funders))
		for fi, funder := range funders {
			wg.Add(1)
			go func() {
				defer wg.Done()
				acct := znn.NewAccount(devnet.Key(funder), e.L1, e.Rollup.L1ChainID)
				for j := range queue {
					var err error
					for attempt := 0; attempt < 60 && ctx.Err() == nil; attempt++ {
						if _, err = acct.Send(ctx, addrs[j], znn.ZNN, want, nil); err == nil {
							break
						}
						if strings.Contains(err.Error(), "insufficient balance") {
							break
						}
						acct.Reset()
						time.Sleep(5 * time.Second)
					}
					if err == nil {
						continue
					}
					if !strings.Contains(err.Error(), "insufficient balance") {
						errs[fi] = fmt.Errorf("sending ZNN to %s: %w", addrs[j], err)
						return
					}
					e.Log.Warn("a funder is spent; what it has not sent goes to the others", "funder", funder)
					mu.Lock()
					failed = append(failed, j)
					for k := range queue {
						failed = append(failed, k)
					}
					mu.Unlock()
					return
				}
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				return err
			}
		}
		if len(failed) == len(unsent) {
			return fmt.Errorf("no L1 funder could pay %d native wallets", len(unsent))
		}
		unsent = failed
	}
	if len(unsent) > 0 {
		return fmt.Errorf("%d native wallets are still unfunded", len(unsent))
	}
	// Each wallet receives its own ZNN before a phase spends it.
	var mu sync.Mutex
	left := map[int]bool{}
	for _, i := range need {
		left[i] = true
	}
	start := time.Now()
	for len(left) > 0 && time.Since(start) < 10*time.Minute && ctx.Err() == nil {
		var todo []int
		for i := range left {
			todo = append(todo, i)
		}
		bench.Parallel(ctx, len(todo), 16, func(k int) error {
			i := todo[k]
			info, err := e.L1.AccountInfo(ctx, addrs[i])
			if err == nil && info.Balance(znn.ZNN).Cmp(low) >= 0 {
				mu.Lock()
				delete(left, i)
				mu.Unlock()
				return nil
			}
			un, err := e.L1.UnreceivedBlocks(ctx, addrs[i], 0, 10)
			if err != nil || len(un.List) == 0 {
				return nil
			}
			acct := znn.NewAccount(devnet.Key(base+uint32(i)), e.L1, e.Rollup.L1ChainID)
			acct.AssumeFused = true
			for _, b := range un.List {
				if _, err := acct.Receive(ctx, b.Hash); err != nil {
					break
				}
			}
			return nil
		})
		if len(left) > 0 {
			time.Sleep(10 * time.Second)
		}
	}
	p.set("nativeFundedSeconds", time.Since(start).Seconds())
	if len(left) > 0 {
		return fmt.Errorf("%d of %d native wallets still hold no ZNN to burn", len(left), len(need))
	}
	return nil
}
