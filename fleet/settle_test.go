package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
)

type settleFixtureOptions struct {
	info, momentum       string
	infoStatus           int
	l1Error, l2Error     bool
	noBaseFee, zeroFloor bool
	baseFee              int64
	blockInfo            bool
	cancelInfo           context.CancelFunc
}

func settleFixtureEnv(t *testing.T, options settleFixtureOptions) *Env {
	t.Helper()
	if options.info == "" {
		options.info = `{"relayer":{"queued":0,"postedPending":0}}`
	}
	if options.momentum == "" {
		options.momentum = `{"version":1,"height":1}`
	}
	header := &types.Header{Number: big.NewInt(1), Difficulty: new(big.Int), Extra: []byte{}, GasLimit: 30000000, Time: 1}
	if !options.noBaseFee {
		fee := options.baseFee
		if fee == 0 {
			fee = 100
		}
		header.BaseFee = big.NewInt(fee)
	}
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/info" {
			if options.cancelInfo != nil {
				options.cancelInfo()
			}
			if options.blockInfo {
				select {
				case <-r.Context().Done():
					return
				case <-stop:
					return
				}
			}
			if options.infoStatus != 0 {
				w.WriteHeader(options.infoStatus)
			}
			io.WriteString(w, options.info)
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "synthetic invalid request", http.StatusBadRequest)
			return
		}
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		switch {
		case r.URL.Path == "/l1" && request.Method == "ledger.getFrontierMomentum":
			if options.l1Error {
				response["error"] = map[string]any{"code": -32000, "message": "synthetic unavailable momentum"}
			} else {
				response["result"] = json.RawMessage(options.momentum)
			}
		case r.URL.Path == "/eth" && request.Method == "eth_getBlockByNumber":
			if options.l2Error {
				response["error"] = map[string]any{"code": -32000, "message": "synthetic unavailable header"}
			} else {
				response["result"] = header
			}
		default:
			response["error"] = map[string]any{"code": -32601, "message": "synthetic method outside fixture"}
		}
		json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(func() { close(stop); srv.Close() })
	eth, err := ethclient.Dial(srv.URL + "/eth")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eth.Close)
	floor := uint64(100)
	if options.zeroFloor {
		floor = 0
	}
	return &Env{API: srv.URL, http: srv.Client(), L1: znn.NewClient(srv.URL + "/l1"), Eth: eth,
		Rollup: &rollup.Config{MinBaseFee: floor}, Out: t.TempDir(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestSettleRequiresUsableObservations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options settleFixtureOptions
		gaveUp  bool
	}{
		{"legacy-calm", settleFixtureOptions{}, false},
		{"dynamic-calm", settleFixtureOptions{momentum: `{"version":2,"height":1,"nextFusionPrice":1000}`}, false},
		{"thresholds", settleFixtureOptions{momentum: `{"version":2,"height":1,"nextFusionPrice":1200}`, baseFee: 120}, false},
		{"no-fee-floor", settleFixtureOptions{zeroFloor: true, noBaseFee: true}, false},
		{"queued", settleFixtureOptions{info: `{"relayer":{"queued":1,"postedPending":0}}`}, true},
		{"posted", settleFixtureOptions{info: `{"relayer":{"queued":0,"postedPending":1}}`}, true},
		{"hot-fusion", settleFixtureOptions{momentum: `{"version":2,"height":1,"nextFusionPrice":1201}`}, true},
		{"hot-base-fee", settleFixtureOptions{baseFee: 121}, true},
		{"info-unavailable", settleFixtureOptions{infoStatus: http.StatusServiceUnavailable}, true},
		{"info-malformed", settleFixtureOptions{info: `{"relayer":`}, true},
		{"info-trailing", settleFixtureOptions{info: `{"relayer":{"queued":0,"postedPending":0}}{}`}, true},
		{"info-null", settleFixtureOptions{info: `null`}, true},
		{"info-empty", settleFixtureOptions{info: `{}`}, true},
		{"relayer-null", settleFixtureOptions{info: `{"relayer":null}`}, true},
		{"posted-missing", settleFixtureOptions{info: `{"relayer":{"queued":0}}`}, true},
		{"queued-missing", settleFixtureOptions{info: `{"relayer":{"postedPending":0}}`}, true},
		{"queued-negative", settleFixtureOptions{info: `{"relayer":{"queued":-1,"postedPending":0}}`}, true},
		{"queued-fractional", settleFixtureOptions{info: `{"relayer":{"queued":0.5,"postedPending":0}}`}, true},
		{"queued-full-width", settleFixtureOptions{info: `{"relayer":{"queued":18446744073709551615,"postedPending":0}}`}, true},
		{"queued-overflow", settleFixtureOptions{info: `{"relayer":{"queued":18446744073709551616,"postedPending":0}}`}, true},
		{"l1-unavailable", settleFixtureOptions{l1Error: true}, true},
		{"l1-null", settleFixtureOptions{momentum: `null`}, true},
		{"l1-empty", settleFixtureOptions{momentum: `{}`}, true},
		{"l1-height-zero", settleFixtureOptions{momentum: `{"version":1,"height":0}`}, true},
		{"dynamic-price-missing", settleFixtureOptions{momentum: `{"version":2,"height":1}`}, true},
		{"dynamic-price-invalid", settleFixtureOptions{momentum: `{"version":2,"height":1,"nextFusionPrice":999}`}, true},
		{"l2-unavailable", settleFixtureOptions{l2Error: true}, true},
		{"base-fee-missing", settleFixtureOptions{noBaseFee: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := settleFixtureEnv(t, tc.options)
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			_, gaveUp := e.settle(ctx, 300*time.Millisecond)
			if gaveUp != tc.gaveUp {
				t.Errorf("settle gaveUp = %v, want %v; unavailable observations cannot establish readiness", gaveUp, tc.gaveUp)
			}
		})
	}
}

func TestSettleProbeRespectsMaximum(t *testing.T) {
	e := settleFixtureEnv(t, settleFixtureOptions{blockInfo: true})
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, gaveUp := e.settle(ctx, 100*time.Millisecond)
	if elapsed := time.Since(start); !gaveUp || elapsed >= time.Second {
		t.Errorf("blocked readiness probe escaped its maximum: gaveUp=%v, elapsed=%s", gaveUp, elapsed)
	}
}

func TestSettleCanceledContextCannotBeReady(t *testing.T) {
	e := settleFixtureEnv(t, settleFixtureOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, gaveUp := e.settle(ctx, time.Second); !gaveUp {
		t.Fatal("canceled readiness check returned settled")
	}
}

func TestRunPhasesStopsWhenSettleIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := settleFixtureEnv(t, settleFixtureOptions{cancelInfo: cancel})
	var ran atomic.Bool
	err := e.RunPhases(ctx, []Phase{{ID: "00-synthetic", Run: func(context.Context, *Env, *PhaseRecord) error {
		ran.Store(true)
		return nil
	}}})
	if !errors.Is(err, context.Canceled) || ran.Load() {
		t.Errorf("phase started after settle cancellation: ran=%v, error=%v", ran.Load(), err)
	}
}
