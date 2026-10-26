package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

func TestUserTimingAvailabilityMatrix(t *testing.T) {
	for _, field := range []string{"latencyMs", "confirmMs", "includeMs"} {
		flag := map[string]string{"latencyMs": "latencyKnown", "confirmMs": "confirmKnown", "includeMs": "includeKnown"}[field]
		for _, tc := range []struct {
			name              string
			value             any
			present, declared bool
			known             any
			want              bool
		}{
			{"positive-legacy", 7, true, false, nil, true}, {"zero-legacy", 0, true, false, nil, false},
			{"measured-zero", 0, true, true, true, true}, {"explicit-unknown", 7, true, true, false, false},
			{"null-value", nil, true, true, true, false}, {"absent-value", nil, false, true, true, false},
			{"null-declaration", 7, true, true, nil, false},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				fields := map[string]any{"block": 1}
				if tc.present {
					fields[field] = tc.value
				}
				if tc.declared {
					fields[flag] = tc.known
				}
				raw, err := json.Marshal(timingRow("tx", 1000, fields))
				if err != nil {
					t.Fatal(err)
				}
				r, err := userRecord(raw)
				if err != nil {
					t.Fatal(err)
				}
				var value int64
				var available *bool
				switch field {
				case "latencyMs":
					value, available = r.LatencyMS, r.LatencyKnown
				case "confirmMs":
					value, available = r.ConfirmMS, r.ConfirmKnown
				case "includeMs":
					value, available = r.IncludeMS, r.IncludeKnown
				}
				if recordTimingKnown(value, available) != tc.want {
					t.Fatal("timing availability was invented or erased")
				}
				wire, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				var replay Record
				if json.Unmarshal(wire, &replay) != nil {
					t.Fatal("timing roundtrip failed")
				}
			})
		}
	}
}

func TestUserTimingExactQuantilesMatchIndependentRanks(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 10, 101} {
		v := &userTimingValues{}
		for i := 0; i < n; i++ {
			v.observe(int64((i*37)%103)-50, true)
		}
		original := append([]int64(nil), v.values...)
		d := v.result("synthetic-clock-offset")
		sort.Slice(original, func(i, j int) bool { return original[i] < original[j] })
		for _, q := range []struct {
			key      string
			num, den int64
		}{{"p50", 1, 2}, {"p90", 9, 10}, {"p99", 99, 100}} {
			rank := new(big.Int).Quo(new(big.Int).Mul(big.NewInt(int64(n-1)), big.NewInt(q.num)), big.NewInt(q.den)).Int64()
			if userTimingExact(d, q.key) != big.NewInt(original[rank]).String() {
				t.Fatal("quantile differed from the independent integer-rank oracle")
			}
		}
		var sum big.Int
		for _, x := range original {
			sum.Add(&sum, big.NewInt(x))
		}
		if d.MeanNumerator != sum.String() || d.MeanDenominator != n || !d.Known || !d.Complete {
			t.Fatal("exact mean or coverage changed")
		}
	}
	maxInt := int(^uint(0) >> 1)
	for _, q := range [][2]int{{1, 2}, {9, 10}, {99, 100}} {
		want := new(big.Int).Quo(new(big.Int).Mul(big.NewInt(int64(maxInt-1)), big.NewInt(int64(q[0]))), big.NewInt(int64(q[1]))).Int64()
		if int64(userTimingRank(maxInt, q[0], q[1])) != want {
			t.Fatal("rank arithmetic overflowed")
		}
	}
}

func TestUserTimingExactFormattingAndFloatCompatibility(t *testing.T) {
	for _, tc := range []struct{ value, want string }{{"0", "0.0"}, {"50", "0.0"}, {"150", "0.2"}, {"250", "0.2"}, {"-50", "0.0"}, {"-150", "-0.2"}, {"-250", "-0.2"}, {"9223372036854775807", "9223372036854775.8"}} {
		if got := userTimingSecs(UserDistribution{Known: true, P50Exact: tc.value}, "p50"); got != tc.want {
			t.Fatalf("exact timing formatting: got %q, want %q", got, tc.want)
		}
	}
	if userTimingSecs(UserDistribution{}, "p50") != "n/a" {
		t.Fatal("missing timing became zero")
	}
	v := &userTimingValues{}
	v.observe(0, true)
	d := v.result("synthetic-elapsed")
	if !d.MeanFloat64Known || !d.QuantilesFloat64Known || d.Mean != 0 || d.N != 1 {
		t.Fatal("measured zero lost compatibility availability")
	}
	v = &userTimingValues{}
	for _, x := range []int64{0, 0, 1} {
		v.observe(x, true)
	}
	d = v.result("synthetic-elapsed")
	if d.MeanFloat64Known || d.MeanNumerator != "1" || d.MeanDenominator != 3 || !d.QuantilesFloat64Known {
		t.Fatal("nonrepresentable mean invented a lossless compatibility value")
	}
}

func TestUserTimingCohortCoverageAcrossScopes(t *testing.T) {
	s := timingSummary(t,
		timingRow("tx", 1000, map[string]any{"block": 1, "ok": false, "reverted": true, "confirmMs": 0, "confirmKnown": true, "includeMs": -500}),
		timingRow("tx", 2000, map[string]any{"block": 2}),
		timingRow("order", 3000, map[string]any{"ok": false, "latencyMs": 0, "latencyKnown": true}),
		timingRow("order", 4000, map[string]any{"latencyMs": 90, "latencyKnown": false}),
		timingRow("payout", 5000, map[string]any{"confirmMs": 0, "confirmKnown": true}),
		timingRow("payout", 6000, map[string]any{"ok": false, "confirmMs": 200}),
		timingRow("read", 7000, map[string]any{"latencyMs": 0, "latencyKnown": true, "ok": false}))
	for _, d := range []UserDistribution{s.Totals.Confirm, s.Profiles[0].Confirm, s.Windows[0].Confirm, s.Totals.Order, s.Profiles[0].Order, s.Windows[0].Order} {
		if d.N != 1 || d.Expected != 2 || d.Complete || !d.Known || d.P50Exact != "0" {
			t.Fatal("partial retained timing cohort was erased or called complete")
		}
	}
	if s.Totals.PayoutDelay.N != 1 || s.Totals.PayoutDelay.Expected != 1 || !s.Totals.PayoutDelay.Complete || s.Totals.Read.N != 1 || s.Totals.Read.P50Exact != "0" {
		t.Fatal("payout/read cohort eligibility changed")
	}
	for _, a := range s.Actions {
		if a.Kind == "order" && (a.Latency.N != 1 || a.Latency.Expected != 2) {
			t.Fatal("order action coverage changed")
		}
	}
	md := UsersMarkdown(s)
	if !strings.Contains(md, "## Timing observations") || !strings.Contains(md, "payout-anchor-to-observation-clock-offset") || !strings.Contains(md, "| 1 | 2 | false |") {
		t.Fatal("Markdown omitted timing basis or retained coverage")
	}
}

func TestUserTimingClockArithmeticAndPayoutAnchorAvailability(t *testing.T) {
	for _, tc := range []struct {
		seconds     int64
		known, want bool
	}{{0, true, true}, {1, true, true}, {-1, true, false}, {math.MaxInt64, true, false}, {1, false, false}} {
		r := userPayoutRecord(1000, tc.seconds, tc.known)
		r.Profile, r.Kind, r.Action, r.OK = "light", "payout", "synthetic-payout", true
		observeUserPayoutDelay(r, 500)
		if (r.ConfirmKnown != nil && *r.ConfirmKnown) != tc.want {
			t.Fatal("payout anchor availability was invented")
		}
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := userRecord(raw); err != nil {
			t.Fatal("payout capture did not replay", err)
		}
	}
	if _, known := userClockDifference(math.MaxInt64, math.MinInt64); known {
		t.Fatal("clock subtraction wrapped")
	}
	if value, known := userClockDifference(100, 500); !known || value != -400 {
		t.Fatal("signed clock offset was clamped")
	}
	raw := []byte(`{"t":1000,"profile":"light","action":"synthetic","kind":"payout","ok":true,"timestampBasis":"payout-observation-start","payoutAnchorKnown":false,"confirmMs":55,"confirmKnown":true}`)
	r, err := userRecord(raw)
	if err != nil || recordTimingKnown(r.ConfirmMS, r.ConfirmKnown) {
		t.Fatal("missing declared payout anchor invented a known delay")
	}
}

func TestUserTimingDeclaredPayoutObservationUsesClientBounds(t *testing.T) {
	dir := contextLegacyFixture(t)
	phase := contextPhase(contextMetadata())
	data := phase["data"].(map[string]any)
	data["simStart"], data["simEnd"] = "1970-01-01T00:00:10Z", "1970-01-01T00:00:11Z"
	contextWriteJSON(t, dir, "00-users-synthetic.json", phase)
	writeSampleAccountingLines(t, filepath.Join(dir, "records.jsonl"), []map[string]any{timingRow("tx", 10000, nil), timingRow("payout", 9000, map[string]any{"timestampBasis": userPayoutObservationBasis, "payoutAnchorKnown": false})})
	if s, err := SummarizeUsers(dir); err == nil || s != nil {
		t.Fatal("declared payout observation escaped client-clock bounds")
	}
}

func TestUserTimingRequestCaptureDeclaresAvailability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("synthetic unavailable"))
	}))
	defer server.Close()
	u := &user{s: &sim{e: &Env{API: server.URL, http: server.Client()}}}
	r := &Record{T: 1000, Profile: "light", Kind: "read", Action: "browse-dex"}
	u.browse(context.Background(), r)
	if r.OK || r.Error == "" || r.LatencyKnown == nil || !*r.LatencyKnown {
		t.Fatal("failed page request lost measured elapsed time")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := userRecord(raw); err != nil {
		t.Fatal(err)
	}
	r = &Record{T: 1000, Profile: "light", Kind: "order", Action: "synthetic-list"}
	u.list(context.Background(), r)
	if r.Skipped == "" || r.LatencyKnown == nil || !*r.LatencyKnown {
		t.Fatal("local order capture lost explicit timing availability")
	}
}

func TestUserTimingBlockHeaderRefusesSignedWrapping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid synthetic RPC")
			return
		}
		header := &types.Header{Number: big.NewInt(1), Difficulty: new(big.Int), Extra: []byte{}, GasLimit: 30000000, Time: math.MaxUint64}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": header})
	}))
	defer server.Close()
	client, err := ethclient.Dial(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	s := &sim{e: &Env{Eth: client}}
	if _, known := s.blockTimeOf(context.Background(), big.NewInt(1)); known {
		t.Fatal("header timestamp wrapped into signed clock time")
	}
	if _, known := s.blockTimeOf(context.Background(), nil); known {
		t.Fatal("missing block number became known")
	}
}

func TestUserTimingRoundtripDoesNotAttachUnrelatedScalars(t *testing.T) {
	raw, err := json.Marshal(Record{T: 1000, Profile: "light", Kind: "read", Action: "synthetic-read"})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"confirmMs":`, `"includeMs":`, `"latencyMs":`, `"payoutAnchorMs":`} {
		if bytes.Contains(raw, []byte(field)) {
			t.Fatal("uninvolved record acquired an unused timing scalar")
		}
	}
}
