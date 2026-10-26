package fleet

import (
	"context"
	"math/big"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/znn"
)

// settlePrice is the existing 1.2x threshold, in go-zenon's thousandths.
const settlePrice = 1_200
const settlePriceWait = 3 * time.Minute

// settleResult records the last observed inputs and the reason the wait ended.
// Read/parse success is not a proof of service health, freshness or finality.
type settleResult struct {
	WaitedSeconds    float64 `json:"waitedSeconds"`
	GaveUp           bool    `json:"gaveUp"`
	Reason           string  `json:"reason"`
	Attempts         int     `json:"attempts"`
	RelayerKnown     bool    `json:"relayerKnown"`
	Queued           uint64  `json:"queued"`
	PostedPending    uint64  `json:"postedPending"`
	FusionPriceKnown bool    `json:"fusionPriceKnown"`
	FusionPrice      uint64  `json:"fusionPrice"`
	BaseFeeKnown     bool    `json:"baseFeeKnown"`
	BaseFeeRequired  bool    `json:"baseFeeRequired"`
	BaseFee          string  `json:"baseFee,omitempty"`
	BaseFeeLimit     string  `json:"baseFeeLimit,omitempty"`
	BaseFeeHigh      bool    `json:"baseFeeHigh"`
	waited           time.Duration
}

func (s settleResult) decision(elapsed time.Duration) string {
	if !s.RelayerKnown || !s.FusionPriceKnown || !s.BaseFeeKnown {
		return ""
	}
	busy := s.Queued > 0 || s.PostedPending > 0
	if !busy && !s.BaseFeeHigh && s.FusionPrice <= settlePrice {
		return "settled"
	}
	// Retain the shorter price-only wait, but only with observed empty queues
	// and usable fee observations. Unknown inputs do not establish that case.
	if !busy && elapsed > settlePriceWait {
		return "price-timeout"
	}
	return ""
}

func (e *Env) observeSettle(ctx context.Context) settleResult {
	var s settleResult
	var info map[string]any
	if e.http != nil && e.getJSON(ctx, "/api/info", &info) == nil {
		relayer, _ := info["relayer"].(map[string]any)
		var queuedKnown, postedKnown bool
		s.Queued, queuedKnown = counterValue(relayer["queued"])
		s.PostedPending, postedKnown = counterValue(relayer["postedPending"])
		s.RelayerKnown = queuedKnown && postedKnown
	}
	if ctx.Err() != nil || e.L1 == nil {
		return s
	}
	m, err := e.L1.FrontierMomentum(ctx)
	if err != nil || m == nil || m.Height == 0 || m.Version == 0 {
		return s
	}
	if m.DynamicPlasma() && (m.NextFusionPrice == nil || *m.NextFusionPrice < znn.PriceScaleFactor) {
		return s
	}
	s.FusionPrice, s.FusionPriceKnown = m.FusionPrice(), true
	if ctx.Err() != nil || e.Eth == nil || e.Rollup == nil {
		return s
	}
	h, err := e.Eth.HeaderByNumber(ctx, nil)
	if err != nil || h == nil {
		return s
	}
	floor := new(big.Int).SetUint64(e.Rollup.MinBaseFeeAt(m.Height))
	s.BaseFeeRequired = floor.Sign() > 0
	if h.BaseFee == nil {
		// A chain with no configured fee floor can have a pre-fee header.
		s.BaseFeeKnown = !s.BaseFeeRequired
		return s
	}
	if h.BaseFee.Sign() < 0 {
		return s
	}
	limit := new(big.Int).Div(new(big.Int).Mul(floor, big.NewInt(settlePrice)), big.NewInt(1000))
	s.BaseFee, s.BaseFeeLimit = h.BaseFee.String(), limit.String()
	s.BaseFeeKnown = true
	s.BaseFeeHigh = s.BaseFeeRequired && h.BaseFee.Cmp(limit) > 0
	return s
}

// settle keeps the original two-result contract for callers that need only
// elapsed time and whether readiness was established.
func (e *Env) settle(ctx context.Context, max time.Duration) (time.Duration, bool) {
	s := e.settleState(ctx, max)
	return s.waited, s.GaveUp
}

func (e *Env) settleState(parent context.Context, max time.Duration) settleResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, max)
	defer cancel()
	var latest settleResult
	attempts := 0
	finish := func(reason string) settleResult {
		latest.waited = time.Since(start)
		latest.WaitedSeconds, latest.GaveUp = latest.waited.Seconds(), reason != "settled"
		latest.Reason, latest.Attempts = reason, attempts
		if e.Log != nil {
			switch reason {
			case "price-timeout":
				e.Log.Warn("starting the phase before prices settled", "fusionPrice", latest.FusionPrice,
					"baseFeeHigh", latest.BaseFeeHigh)
			case "timeout":
				e.Log.Warn("chain readiness was not established before the phase", "relayerKnown", latest.RelayerKnown,
					"queued", latest.Queued, "posted", latest.PostedPending, "fusionPriceKnown", latest.FusionPriceKnown,
					"fusionPrice", latest.FusionPrice, "baseFeeKnown", latest.BaseFeeKnown, "baseFeeHigh", latest.BaseFeeHigh)
			}
		}
		return latest
	}
	interrupted := func() settleResult {
		if parent.Err() != nil {
			return finish("canceled")
		}
		return finish("timeout")
	}
	for {
		if ctx.Err() != nil {
			return interrupted()
		}
		attempts++
		latest = e.observeSettle(ctx)
		if ctx.Err() != nil {
			return interrupted()
		}
		if reason := latest.decision(time.Since(start)); reason != "" {
			return finish(reason)
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return interrupted()
		case <-timer.C:
		}
	}
}
