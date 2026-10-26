package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func TestSettlePreservesPriceAndBacklogPolicies(t *testing.T) {
	known := settleResult{RelayerKnown: true, FusionPriceKnown: true, BaseFeeKnown: true, FusionPrice: 1000}
	for _, tc := range []struct {
		name    string
		input   settleResult
		elapsed time.Duration
		want    string
	}{
		{"calm", known, 0, "settled"},
		{"calm-after-long-wait", known, time.Hour, "settled"},
		{"fusion-before-price-budget", settleResult{RelayerKnown: true, FusionPriceKnown: true, BaseFeeKnown: true, FusionPrice: 1201}, time.Minute, ""},
		{"fusion-at-price-budget", settleResult{RelayerKnown: true, FusionPriceKnown: true, BaseFeeKnown: true, FusionPrice: 1201}, 3 * time.Minute, ""},
		{"fusion-after-price-budget", settleResult{RelayerKnown: true, FusionPriceKnown: true, BaseFeeKnown: true, FusionPrice: 1201}, 3*time.Minute + 1, "price-timeout"},
		{"base-fee-after-price-budget", settleResult{RelayerKnown: true, FusionPriceKnown: true, BaseFeeKnown: true, FusionPrice: 1000, BaseFeeHigh: true}, time.Hour, "price-timeout"},
		{"backlog-after-price-budget", settleResult{RelayerKnown: true, Queued: 1, FusionPriceKnown: true, BaseFeeKnown: true, FusionPrice: 1000}, time.Hour, ""},
		{"posted-after-price-budget", settleResult{RelayerKnown: true, PostedPending: 1, FusionPriceKnown: true, BaseFeeKnown: true, FusionPrice: 1000}, time.Hour, ""},
		{"relayer-unknown", settleResult{FusionPriceKnown: true, BaseFeeKnown: true, FusionPrice: 1000}, time.Hour, ""},
		{"fusion-unknown", settleResult{RelayerKnown: true, BaseFeeKnown: true}, time.Hour, ""},
		{"base-fee-unknown", settleResult{RelayerKnown: true, FusionPriceKnown: true, FusionPrice: 1000}, time.Hour, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.input.decision(tc.elapsed); got != tc.want {
				t.Fatalf("readiness decision = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSettleRecordsUsableImmediateResult(t *testing.T) {
	e := settleFixtureEnv(t, settleFixtureOptions{})
	result := e.settleState(context.Background(), time.Second)
	if result.GaveUp || result.Reason != "settled" || result.Attempts != 1 || result.WaitedSeconds < 0 {
		t.Fatal("immediate readiness lost its explicit outcome or attempt count")
	}
	if !result.RelayerKnown || !result.FusionPriceKnown || !result.BaseFeeKnown || !result.BaseFeeRequired {
		t.Fatal("readiness result lost its observed input availability")
	}
	if result.FusionPrice != 1000 || result.BaseFee != "100" || result.BaseFeeLimit != "120" {
		t.Fatal("readiness result did not preserve exact fee inputs")
	}
	phase := &PhaseRecord{ID: "00-synthetic", Data: map[string]any{"settle": result}}
	raw, err := json.Marshal(phase)
	if err != nil {
		t.Fatal(err)
	}
	var decoded PhaseRecord
	if err := decodeMeasurementJSON(bytes.NewReader(raw), &decoded); err != nil {
		t.Fatal(err)
	}
	stored, ok := decoded.Data["settle"].(map[string]any)
	if !ok || stored["reason"] != "settled" || stored["gaveUp"] != false || stored["baseFee"] != "100" || stored["relayerKnown"] != true {
		t.Fatal("phase serialization lost the immediate readiness evidence")
	}
}

func TestSettleUnknownResultRemainsTimeout(t *testing.T) {
	e := settleFixtureEnv(t, settleFixtureOptions{info: `null`})
	result := e.settleState(context.Background(), 50*time.Millisecond)
	if !result.GaveUp || result.Reason != "timeout" || result.RelayerKnown || result.Attempts != 1 {
		t.Fatal("unavailable input did not retain an explicit timeout outcome")
	}
}

func TestSettleCanceledOrExhaustedBudgetDoesNotProbe(t *testing.T) {
	e := &Env{}
	exhausted := e.settleState(context.Background(), 0)
	if !exhausted.GaveUp || exhausted.Reason != "timeout" || exhausted.Attempts != 0 {
		t.Fatal("exhausted readiness budget attempted a probe or returned ready")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := e.settleState(ctx, time.Second)
	if !canceled.GaveUp || canceled.Reason != "canceled" || canceled.Attempts != 0 {
		t.Fatal("caller cancellation was confused with readiness or a wait timeout")
	}
}

func TestRunPhasesCanceledBeforeStartWritesNoPhase(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := settleFixtureEnv(t, settleFixtureOptions{cancelInfo: cancel})
	err := e.RunPhases(ctx, []Phase{{ID: "00-synthetic", Run: func(context.Context, *Env, *PhaseRecord) error {
		t.Error("canceled phase started")
		return nil
	}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("phase cancellation was not returned to the caller")
	}
	entries, err := os.ReadDir(e.Out)
	if err != nil || len(entries) != 0 {
		t.Fatal("canceled phase wrote a result or state file")
	}
}
