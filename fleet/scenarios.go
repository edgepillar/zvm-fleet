package fleet

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Zenoglyphs-VM/zvm-core/devnet"
	"github.com/Zenoglyphs-VM/zvm-node/bench"
	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
)

// The application workloads: what people do on /swap, /nft and /farm, as
// ordinary transactions from ordinary wallets. Their gas limit is 0, so the
// sender estimates each call and adds 30 %, as MetaMask does: a fixed limit
// sized from an average undercuts every wallet's first call, which writes
// fresh storage — the first run of these phases reverted all 14,390 swaps and
// mints that way.

var (
	topicTransfer          = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	topicCollectionCreated = crypto.Keccak256Hash([]byte("CollectionCreated(address,address,string,string)"))
	maxUint256             = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
)

func deadline() *big.Int { return big.NewInt(time.Now().Add(time.Hour).Unix()) }

func (e *Env) funder(ctx context.Context) (*bench.Sender, error) {
	return bench.NewSender(ctx, devnet.EVMKey(bench.FunderEVMKey), e.Eth, e.Submit, e.Rollup)
}

// call sends one transaction and waits for it to succeed.
func (e *Env) call(ctx context.Context, s *bench.Sender, to common.Address, value *big.Int, gas uint64,
	artifact, method string, args ...any) (*types.Receipt, error) {
	data, err := e.Arts.MustGet(artifact).Call(method, args...)
	if err != nil {
		return nil, err
	}
	if value == nil {
		value = new(big.Int)
	}
	tx, err := s.Send(ctx, &to, value, data, gas)
	if err != nil {
		return nil, fmt.Errorf("%s.%s: %w", artifact, method, err)
	}
	return bench.WaitReceipt(ctx, e.Eth, tx, 5*time.Minute)
}

func (e *Env) read(ctx context.Context, to common.Address, artifact, method string, args ...any) ([]any, error) {
	art := e.Arts.MustGet(artifact)
	data, err := art.Call(method, args...)
	if err != nil {
		return nil, err
	}
	out, err := e.Eth.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("%s.%s: %w", artifact, method, err)
	}
	return art.ABI.Unpack(method, out)
}

func (e *Env) pairOf(ctx context.Context, a, b common.Address) (common.Address, error) {
	out, err := e.read(ctx, e.Dex.Factory, "UniswapV2Factory", "getPair", a, b)
	if err != nil {
		return common.Address{}, err
	}
	pair := out[0].(common.Address)
	if pair == (common.Address{}) {
		return pair, fmt.Errorf("no pool for %s/%s — run `zvm dex seed`", a, b)
	}
	return pair, nil
}

// collection is the benchmark's own NFT collection: a clone from the factory
// with a free drop that never closes, so any wallet can mint.
func (e *Env) collection(ctx context.Context) (common.Address, error) {
	if c := e.state.Collection; c != (common.Address{}) {
		if code, err := e.Eth.CodeAt(ctx, c, nil); err == nil && len(code) > 0 {
			return c, nil
		}
	}
	s, err := e.funder(ctx)
	if err != nil {
		return common.Address{}, err
	}
	r, err := e.call(ctx, s, e.Apps.CollectionFactory, nil, 900_000, "ZVMCollectionFactory", "createCollection",
		"ZVM Fleet", "FLEET", "", "", new(big.Int))
	if err != nil {
		return common.Address{}, err
	}
	var coll common.Address
	for _, l := range r.Logs {
		if l.Address == e.Apps.CollectionFactory && len(l.Topics) == 3 && l.Topics[0] == topicCollectionCreated {
			coll = common.BytesToAddress(l.Topics[1].Bytes())
		}
	}
	if coll == (common.Address{}) {
		return coll, errors.New("the factory created no collection")
	}
	start := uint32(time.Now().Add(-time.Hour).Unix())
	if _, err := e.call(ctx, s, coll, nil, 200_000, "ZVMCollection", "setDrop",
		new(big.Int), uint32(0), uint32(0), start, uint32(0)); err != nil {
		return coll, err
	}
	e.state.Collection = coll
	e.Log.Info("fleet collection", "address", coll, "gas", r.GasUsed)
	return coll, e.saveState()
}

func (e *Env) swapScenario(ctx context.Context) (*bench.Scenario, error) {
	if _, err := e.pairOf(ctx, e.Dex.WETH, e.Dex.ERC20); err != nil {
		return nil, err
	}
	router := e.Arts.MustGet("UniswapV2Router02")
	return &bench.Scenario{Name: "swap-znn", Gas: 0,
		Build: func(d *bench.Deployment, _ *bench.Artifacts, w *bench.Wallet) (*common.Address, *big.Int, []byte, error) {
			data, err := router.Call("swapExactETHForTokens", new(big.Int), []common.Address{d.WETH, d.ERC20}, w.Addr, deadline())
			to := d.Router
			return &to, wei(0.001), data, err
		}}, nil
}

func (e *Env) mintScenario(ctx context.Context) (*bench.Scenario, error) {
	coll, err := e.collection(ctx)
	if err != nil {
		return nil, err
	}
	data, err := e.Arts.MustGet("ZVMCollection").Call("publicMint", big.NewInt(1))
	if err != nil {
		return nil, err
	}
	return &bench.Scenario{Name: "nft-mint", Gas: 0,
		Build: func(_ *bench.Deployment, _ *bench.Artifacts, _ *bench.Wallet) (*common.Address, *big.Int, []byte, error) {
			to := coll
			return &to, new(big.Int), data, nil
		}}, nil
}

// listScenario mints three tokens to every wallet and approves the exchange,
// then lists them over and over with approveOrders — one on-chain listing per
// transaction. A signed listing costs the chain nothing at all (D28), so the
// transaction a listing can still be is the one this measures: the path a
// contract wallet or a Zenon account uses, Seaport's validate.
func (e *Env) listScenario(ctx context.Context, base uint32) (*bench.Scenario, error) {
	coll, err := e.collection(ctx)
	if err != nil {
		return nil, err
	}
	exchange := e.Apps.Exchange
	nft := e.Arts.MustGet("ZVMCollection")
	ex := e.Arts.MustGet("ZVMExchange")
	const perWallet = 3
	sc := &bench.Scenario{Name: "nft-list", Gas: 0}
	// Owned tokens are keyed by the wallet's absolute key index, so a later
	// run from another range cannot mistake them for its own.
	key := func(w *bench.Wallet) int { return int(base) + w.Index }
	sc.Prepare = func(ctx context.Context, cfg bench.RunConfig, wallets []*bench.Wallet) error {
		mint, err := nft.Call("publicMint", big.NewInt(perWallet))
		if err != nil {
			return err
		}
		approve, err := nft.Call("setApprovalForAll", exchange, true)
		if err != nil {
			return err
		}
		mints := make([]*types.Transaction, len(wallets))
		approvals := make([]*types.Transaction, len(wallets))
		for i, w := range wallets {
			if len(e.owned[key(w)]) >= perWallet {
				continue
			}
			if mints[i], err = w.Sender.Send(ctx, &coll, new(big.Int), mint, 400_000); err != nil {
				return err
			}
			if approvals[i], err = w.Sender.Send(ctx, &coll, new(big.Int), approve, 80_000); err != nil {
				return err
			}
		}
		e.Log.Info("minting and approving for listings", "wallets", len(wallets))
		var mu sync.Mutex
		err = bench.Parallel(ctx, len(wallets), 32, func(i int) error {
			if mints[i] == nil {
				return nil
			}
			r, err := bench.WaitReceipt(ctx, e.Eth, mints[i], 10*time.Minute)
			if err != nil {
				return err
			}
			if _, err := bench.WaitReceipt(ctx, e.Eth, approvals[i], 10*time.Minute); err != nil {
				return err
			}
			var ids []*big.Int
			for _, l := range r.Logs {
				if l.Address == coll && len(l.Topics) == 4 && l.Topics[0] == topicTransfer {
					ids = append(ids, l.Topics[3].Big())
				}
			}
			mu.Lock()
			e.owned[key(wallets[i])] = ids
			mu.Unlock()
			return nil
		})
		if err != nil {
			return err
		}
		return e.saveState()
	}
	sc.Build = func(_ *bench.Deployment, _ *bench.Artifacts, w *bench.Wallet) (*common.Address, *big.Int, []byte, error) {
		ids := e.owned[key(w)]
		if len(ids) == 0 {
			return nil, nil, nil, errors.New("wallet holds no token to list")
		}
		now := time.Now()
		o := &orderbook.Order{
			Trader: w.Addr, Side: orderbook.SideSell, Collection: coll, TokenID: orderbook.B(ids[w.Seq%len(ids)]),
			Amount: orderbook.U(1), Price: orderbook.B(wei(0.001)),
			ListingTime: uint64(now.Add(-5 * time.Minute).Unix()), ExpirationTime: uint64(now.Add(24 * time.Hour).Unix()),
			Nonce: orderbook.U(0), Salt: orderbook.U(int64(key(w))<<32 | int64(w.Seq)),
		}
		data, err := ex.Call("approveOrders", orderbook.ABIOrders([]*orderbook.Order{o}))
		to := exchange
		return &to, new(big.Int), data, err
	}
	return sc, nil
}

// buyScenario buys one fillable listing of every token the list phase put up,
// each exactly once, from a separate set of wallets. The listings come from the
// node's order book, which found them in the exchange's logs.
func (e *Env) buyScenario(ctx context.Context) (*bench.Scenario, int, error) {
	coll := e.state.Collection
	if coll == (common.Address{}) || len(e.owned) == 0 {
		return nil, 0, errors.New("nothing listed: run the nft-list phase first")
	}
	var rows []struct {
		Hash    string `json:"hash"`
		TokenID string `json:"tokenId"`
	}
	if err := e.getJSON(ctx, fmt.Sprintf("/api/nft/orders?collection=%s&side=sell&limit=500", coll.Hex()), &rows); err != nil {
		return nil, 0, err
	}
	seen := map[string]bool{}
	var hashes []string
	for _, r := range rows {
		if !seen[r.TokenID] {
			seen[r.TokenID] = true
			hashes = append(hashes, r.Hash)
		}
	}
	var mu sync.Mutex
	var open []*orderbook.Input
	err := bench.Parallel(ctx, len(hashes), 16, func(i int) error {
		var one struct {
			Input    *orderbook.Input `json:"input"`
			Fillable bool             `json:"fillable"`
		}
		if err := e.getJSON(ctx, "/api/nft/orders/"+hashes[i], &one); err != nil {
			return err
		}
		if one.Fillable && one.Input != nil {
			mu.Lock()
			open = append(open, one.Input)
			mu.Unlock()
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	queue := make(chan *orderbook.Input, len(open))
	for _, in := range open {
		queue <- in
	}
	ex := e.Arts.MustGet("ZVMExchange")
	exchange := e.Apps.Exchange
	return &bench.Scenario{Name: "nft-buy", Gas: 0,
		Build: func(_ *bench.Deployment, _ *bench.Artifacts, _ *bench.Wallet) (*common.Address, *big.Int, []byte, error) {
			select {
			case in := <-queue:
				data, err := ex.Call("buy", in.ABI())
				to := exchange
				return &to, in.Order.Price.Big(), data, err
			default:
				return nil, nil, nil, errors.New("no listings left")
			}
		}}, len(open), nil
}

// farmScenario gives every wallet ZNN/BNA liquidity and then alternates
// staking a sliver of it with harvesting.
func (e *Env) farmScenario(ctx context.Context) (*bench.Scenario, error) {
	pid, pair, err := e.farmPool(ctx)
	if err != nil {
		return nil, err
	}
	farm := e.Arts.MustGet("ZVMFarm")
	router := e.Arts.MustGet("UniswapV2Router02")
	erc20 := e.Arts.MustGet("BenchERC20")
	// 150 k covers the dearest path measured (127 k: a harvest that pays a
	// reward to a first-time holder) with room; see bench.Scenario.MinGas.
	sc := &bench.Scenario{Name: "farm", Gas: 0, MinGas: 150_000, NeedsToken: true}
	sc.Prepare = func(ctx context.Context, cfg bench.RunConfig, wallets []*bench.Wallet) error {
		approveRouter, err := erc20.Call("approve", e.Dex.Router, maxUint256)
		if err != nil {
			return err
		}
		approveFarm, err := erc20.Call("approve", e.Apps.Farm, maxUint256)
		if err != nil {
			return err
		}
		last := make([]*types.Transaction, len(wallets))
		for i, w := range wallets {
			if _, err := w.Sender.Send(ctx, &e.Dex.ERC20, new(big.Int), approveRouter, 80_000); err != nil {
				return err
			}
			add, err := router.Call("addLiquidityETH", e.Dex.ERC20, wei(1000), new(big.Int), new(big.Int), w.Addr, deadline())
			if err != nil {
				return err
			}
			if _, err := w.Sender.Send(ctx, &e.Dex.Router, wei(1), add, 400_000); err != nil {
				return err
			}
			if last[i], err = w.Sender.Send(ctx, &pair, new(big.Int), approveFarm, 80_000); err != nil {
				return err
			}
		}
		e.Log.Info("adding liquidity and approving the farm", "wallets", len(wallets), "pid", pid)
		return bench.Parallel(ctx, len(last), 32, func(i int) error {
			_, err := bench.WaitReceipt(ctx, e.Eth, last[i], 10*time.Minute)
			return err
		})
	}
	sc.Build = func(_ *bench.Deployment, _ *bench.Artifacts, w *bench.Wallet) (*common.Address, *big.Int, []byte, error) {
		var data []byte
		var err error
		if w.Seq%2 == 0 {
			data, err = farm.Call("deposit", big.NewInt(int64(pid)), wei(0.001), w.Addr)
		} else {
			data, err = farm.Call("harvest", big.NewInt(int64(pid)), w.Addr)
		}
		to := e.Apps.Farm
		return &to, new(big.Int), data, err
	}
	return sc, nil
}

// farmPool finds the farm's pool for the ZNN/BNA pair: its id and the LP
// token it stakes.
func (e *Env) farmPool(ctx context.Context) (int, common.Address, error) {
	pair, err := e.pairOf(ctx, e.Dex.ERC20, e.Dex.WETH)
	if err != nil {
		return 0, pair, err
	}
	farm := e.Arts.MustGet("ZVMFarm")
	n, err := e.read(ctx, e.Apps.Farm, "ZVMFarm", "poolLength")
	if err != nil {
		return 0, pair, err
	}
	for i := int64(0); i < n[0].(*big.Int).Int64(); i++ {
		data, err := farm.Call("poolInfo", big.NewInt(i))
		if err != nil {
			return 0, pair, err
		}
		out, err := e.Eth.CallContract(ctx, ethereum.CallMsg{To: &e.Apps.Farm, Data: data}, nil)
		if err == nil && len(out) >= 32 && common.BytesToAddress(out[12:32]) == pair {
			return int(i), pair, nil
		}
	}
	return 0, pair, fmt.Errorf("the farm has no pool for the ZNN/BNA pair %s", pair)
}

// noop funds wallets without sending anything: a scenario with no Build.
var noop = &bench.Scenario{Name: "fund"}
