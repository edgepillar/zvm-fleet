package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-bridge/bridge"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func timingRow(kind string, t int64, fields map[string]any) map[string]any {
	r := map[string]any{"t": t, "user": 0, "profile": "light", "kind": kind, "action": "synthetic-timing", "ok": true}
	for k, v := range fields {
		r[k] = v
	}
	return r
}

func timingSummary(t *testing.T, rows ...map[string]any) *UsersSummary {
	t.Helper()
	dir := t.TempDir()
	writeSampleAccountingLines(t, filepath.Join(dir, "records.jsonl"), rows)
	s, err := SummarizeUsers(dir)
	if err != nil {
		t.Fatal("timing replay refused retained observations", err)
	}
	return s
}

func timingJSON(t *testing.T, value any, keys ...string) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var current any
	if json.Unmarshal(raw, &current) != nil {
		t.Fatal("invalid timing JSON")
	}
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	object, _ := current.(map[string]any)
	return object
}

func TestUserTimingUnknownReadDoesNotInventZero(t *testing.T) {
	for _, kind := range []string{"missing", "null", "false", "declared-without-value"} {
		t.Run(kind, func(t *testing.T) {
			fields := map[string]any{}
			switch kind {
			case "null":
				fields["latencyMs"] = nil
			case "false":
				fields["latencyMs"], fields["latencyKnown"] = 80, false
			case "declared-without-value":
				fields["latencyKnown"] = true
			}
			s := timingSummary(t, timingRow("read", 1000, map[string]any{"latencyMs": 40}), timingRow("read", 2000, fields))
			if s.Totals.Read.N != 1 || s.Totals.Read.P50 != 40 || s.Profiles[0].Latency.N != 1 || s.Actions[0].Latency.N != 1 || s.Windows[0].Read.N != 1 {
				t.Fatal("unknown request timing invented a zero or usable sample")
			}
			j := timingJSON(t, s.Totals.Read)
			if j["known"] != true || j["complete"] != false || j["expected"] != float64(2) {
				t.Fatal("request timing coverage was not retained")
			}
		})
	}
}

func TestUserTimingMeasuredZeroSurvivesCaptureReplay(t *testing.T) {
	for _, field := range []string{"latencyMs", "confirmMs", "includeMs"} {
		t.Run(field, func(t *testing.T) {
			flag := map[string]string{"latencyMs": "latencyKnown", "confirmMs": "confirmKnown", "includeMs": "includeKnown"}[field]
			kind := "tx"
			if field == "latencyMs" {
				kind = "read"
			}
			raw, err := json.Marshal(timingRow(kind, 1000, map[string]any{"block": 1, field: 0, flag: true}))
			if err != nil {
				t.Fatal(err)
			}
			var r Record
			if json.Unmarshal(raw, &r) != nil {
				t.Fatal("measured timing zero did not decode")
			}
			wire, err := json.Marshal(r)
			if err != nil || !bytes.Contains(wire, []byte(`"`+field+`":0`)) {
				t.Fatal("measured timing zero disappeared during capture/replay")
			}
			s := timingSummary(t, timingRow(kind, 1000, map[string]any{"block": 1, field: 0, flag: true}))
			key := map[string]string{"latencyMs": "readMs", "confirmMs": "confirmMs", "includeMs": "includeMs"}[field]
			j := timingJSON(t, s.Totals, key)
			if j["known"] != true || j["n"] != float64(1) || j["p50Exact"] != "0" {
				t.Fatal("measured timing zero was not available in the distribution")
			}
		})
	}
}

func TestUserTimingSignedClockOffsetsRemainSeparate(t *testing.T) {
	for _, kind := range []string{"tx", "payout"} {
		t.Run(kind, func(t *testing.T) {
			fields := map[string]any{"confirmMs": 20, "block": 1, "includeMs": -300, "includeKnown": true}
			if kind == "payout" {
				fields = map[string]any{"confirmMs": -300, "confirmKnown": true}
			}
			s := timingSummary(t, timingRow(kind, 1000, fields))
			key, basis := "includeMs", "submit-to-block-clock-offset"
			if kind == "payout" {
				key, basis = "payoutDelayMs", "payout-anchor-to-observation-clock-offset"
				if s.Totals.Confirm.N != 0 || s.Actions[0].Confirm.N != 0 {
					t.Fatal("payout clock offset became transaction confirmation")
				}
			}
			j := timingJSON(t, s.Totals, key)
			if j["p50Exact"] != "-300" || j["basis"] != basis {
				t.Fatal("signed clock offset was lost or mislabeled as elapsed latency")
			}
		})
	}
}

func TestUserTimingCohortsDoNotMixRoles(t *testing.T) {
	s := timingSummary(t,
		timingRow("tx", 1000, map[string]any{"block": 1, "confirmMs": 20}),
		timingRow("tx", 2000, map[string]any{"confirmMs": 5000}),
		timingRow("read", 3000, map[string]any{"confirmMs": 8000, "latencyMs": 8}),
		timingRow("order", 4000, map[string]any{"confirmMs": 7000, "latencyMs": 9, "ok": false, "error": "synthetic order refusal"}),
		timingRow("payout", 5000, map[string]any{"confirmMs": 1000}),
		timingRow("tx", 6000, map[string]any{"block": 2, "confirmMs": 9000, "skipped": "synthetic skip"}))
	if s.Totals.Confirm.N != 1 || s.Profiles[0].Confirm.N != 1 || s.Windows[0].Confirm.N != 1 {
		t.Fatal("confirmation timing admitted a nonincluded or skipped transaction")
	}
	for _, a := range s.Actions {
		if a.Kind != "tx" && a.Confirm.N != 0 {
			t.Error("nontransaction timing entered confirmation distribution")
		}
		if a.Kind == "order" && (a.Latency.N != 1 || a.Latency.P50 != 9) {
			t.Error("failed order request timing was discarded")
		}
	}
}

func TestUserTimingWideDistributionRetainsExactOperands(t *testing.T) {
	s := timingSummary(t, timingRow("read", 1000, map[string]any{"latencyMs": int64(1)}), timingRow("read", 2000, map[string]any{"latencyMs": int64(math.MaxInt64 - 2)}), timingRow("read", 3000, map[string]any{"latencyMs": int64(math.MaxInt64)}))
	j := timingJSON(t, s.Totals.Read)
	if j["p50Exact"] != "9223372036854775805" || j["maxExact"] != "9223372036854775807" || j["meanNumerator"] != "18446744073709551613" || j["meanDenominator"] != float64(3) || j["quantilesFloat64Known"] != false || j["meanFloat64Known"] != false {
		t.Fatal("timing distribution lost exact quantiles or rational mean operands")
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var replay UsersSummary
	if json.Unmarshal(raw, &replay) != nil || timingJSON(t, replay.Totals.Read)["p50Exact"] != j["p50Exact"] {
		t.Fatal("serialized timing summary lost exact operands")
	}
}

func TestUserTimingMissingRendersUnavailable(t *testing.T) {
	s := timingSummary(t, timingRow("read", 1000, nil))
	if !strings.Contains(UsersMarkdown(s), "| Page load p50 / p90 / p99, ms | n/a / n/a / n/a |") {
		t.Fatal("missing timing rendered as measured zero or blank")
	}
}

func TestUserTimingAliasesAreRejected(t *testing.T) {
	for _, field := range []string{"LATENCYMS", "ConfirmKnown", "INCLUDEKNOWN"} {
		t.Run(field, func(t *testing.T) {
			raw, err := json.Marshal(timingRow("tx", 1000, map[string]any{field: 1, "block": 1}))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := userRecord(raw); err == nil {
				t.Fatal("noncanonical timing field was accepted")
			}
		})
	}
}

func TestUserTimingUnavailablePayoutAnchorRetainsReplayableFailure(t *testing.T) {
	e := settleFixtureEnv(t, settleFixtureOptions{})
	s, ctx, cancel, _ := writerSimulation(t, 0)
	s.e.Eth = e.Eth
	cancel(errors.New("synthetic unavailable payout anchor"))
	u := &user{s: s, p: Profiles[0]}
	u.followWithdrawal(ctx, &types.Receipt{BlockNumber: big.NewInt(1), Logs: []*types.Log{{Address: rollup.BridgeAddr, Topics: []common.Hash{bridge.TopicWithdrawalInitiated, common.BigToHash(big.NewInt(1)), {}, {}}}}})
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("unavailable payout anchor did not terminate")
	}
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.e.Out, "records.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := userRecord(bytes.TrimSpace(raw)); err != nil {
		t.Fatal("unavailable payout anchor produced an unreplayable terminal record")
	}
}
