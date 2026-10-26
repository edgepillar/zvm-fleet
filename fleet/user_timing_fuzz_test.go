package fleet

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Failure traces contain only bounded synthetic arguments. Recover and re-panic
// preserve panic failures while making their effective input reproducible from
// the retained job log; no per-input filesystem work or corpus upload is needed.
func timingFuzzTrace(t *testing.T, raw []byte) {
	cause := recover()
	if cause != nil || t.Failed() {
		t.Logf("synthetic timing reproduction prefix: %x", raw)
	}
	if cause != nil {
		panic(cause)
	}
}

// These oracles do not call the production availability/rank/format helpers.
// Bounded rationals are normal binary64 numbers when representable: the reduced
// denominator must be a power of two and the numerator needs at most 53 bits
// after removing powers of two. This avoids sharing Rat.Float64's exact flag.
func timingFuzzBinaryExact(numerator *big.Int, denominator int) bool {
	if numerator.Sign() == 0 {
		return true
	}
	n := new(big.Int).Abs(numerator)
	d := big.NewInt(int64(denominator))
	gcd := new(big.Int).GCD(nil, nil, n, d)
	n.Quo(n, gcd)
	d.Quo(d, gcd)
	power := new(big.Int).And(new(big.Int).Set(d), new(big.Int).Sub(new(big.Int).Set(d), big.NewInt(1))).Sign() == 0
	return power && n.BitLen()-int(n.TrailingZeroBits()) <= 53
}

// Choose the closest adjacent decimal tick by exact rational distances. Ties
// select the even tick, including signed ties and suppression of negative zero.
func timingFuzzDecimal(numerator *big.Int, denominator int64, decimals int) string {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	scaled := new(big.Rat).SetFrac(new(big.Int).Mul(numerator, scale), big.NewInt(denominator))
	lower := new(big.Int).Div(scaled.Num(), scaled.Denom())
	upper := new(big.Int).Add(new(big.Int).Set(lower), big.NewInt(1))
	lowDistance := new(big.Rat).Abs(new(big.Rat).Sub(scaled, new(big.Rat).SetInt(lower)))
	highDistance := new(big.Rat).Abs(new(big.Rat).Sub(new(big.Rat).SetInt(upper), scaled))
	selected := lower
	if order := highDistance.Cmp(lowDistance); order < 0 || order == 0 && upper.Bit(0) == 0 {
		selected = upper
	}
	out := new(big.Int).Abs(selected).String()
	if decimals > 0 {
		if len(out) <= decimals {
			out = strings.Repeat("0", decimals+1-len(out)) + out
		}
		out = out[:len(out)-decimals] + "." + out[len(out)-decimals:]
	}
	if selected.Sign() < 0 {
		out = "-" + out
	}
	return out
}

func timingFuzzDistribution(t *testing.T, got UserDistribution, input []int64, expected int, basis string) {
	t.Helper()
	values := append([]int64(nil), input...)
	sort.SliceStable(values, func(i, j int) bool { return values[i] < values[j] })
	n := len(values)
	if got.N != n || got.Expected != expected || got.Known != (n > 0) || got.Complete != (n > 0 && n == expected) ||
		got.Basis != basis || got.QuantilePolicy != "lower-index-floor((n-1)*p)" {
		t.Fatal("timing eligibility, coverage or basis differs from the independent cohort")
	}
	exact := []string{got.MinExact, got.P50Exact, got.P90Exact, got.P99Exact, got.MaxExact}
	if n == 0 {
		for _, text := range exact {
			if text != "" {
				t.Fatal("empty timing cohort invented an exact value")
			}
		}
		if got.MeanNumerator != "" || got.MeanDenominator != 0 || got.MeanFloat64Known || got.QuantilesFloat64Known ||
			got.Mean != 0 || got.P50 != 0 || got.P90 != 0 || got.P99 != 0 || got.Max != 0 {
			t.Fatal("empty timing cohort invented a mean or compatibility observation")
		}
		return
	}
	ranks := []int{0}
	for _, ratio := range [][2]int64{{1, 2}, {9, 10}, {99, 100}} {
		index := new(big.Int).Quo(new(big.Int).Mul(big.NewInt(int64(n-1)), big.NewInt(ratio[0])), big.NewInt(ratio[1]))
		ranks = append(ranks, int(index.Int64()))
	}
	ranks = append(ranks, n-1)
	compatibility := true
	for i, rank := range ranks {
		value := values[rank]
		if exact[i] != strconv.FormatInt(value, 10) {
			t.Fatal("exact timing order statistic differs")
		}
		compatibility = compatibility && timingFuzzBinaryExact(big.NewInt(value), 1)
	}
	if got.QuantilesFloat64Known != compatibility {
		t.Fatal("quantile binary64 availability differs")
	}
	floats := []float64{got.P50, got.P90, got.P99, got.Max}
	for i, value := range floats {
		want := float64(0)
		if compatibility {
			want = float64(values[ranks[i+1]])
		}
		if value != want {
			t.Fatal("quantile compatibility value differs")
		}
	}
	var sum big.Int
	for _, value := range values {
		sum.Add(&sum, big.NewInt(value))
	}
	if got.MeanNumerator != sum.String() || got.MeanDenominator != n {
		t.Fatal("exact unreduced mean operands differ")
	}
	meanExact := timingFuzzBinaryExact(&sum, n)
	if got.MeanFloat64Known != meanExact {
		t.Fatal("mean binary64 availability differs")
	}
	mean := float64(0)
	if meanExact {
		numerator := new(big.Float).SetPrec(256).SetInt(&sum)
		denominator := new(big.Float).SetPrec(256).SetInt64(int64(n))
		mean, _ = new(big.Float).SetPrec(256).Quo(numerator, denominator).Float64()
	}
	if got.Mean != mean {
		t.Fatal("mean compatibility value differs")
	}
	for i, name := range []string{"min", "p50", "p90", "p99", "max"} {
		value := values[ranks[i]]
		if userTimingMS(got, name) != strconv.FormatInt(value, 10) ||
			userTimingSecs(got, name) != timingFuzzDecimal(big.NewInt(value), 1000, 1) {
			t.Fatal("exact timing display differs from rational nearest-even ticks")
		}
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var replay UserDistribution
	if json.Unmarshal(encoded, &replay) != nil || replay != got {
		t.Fatal("distribution JSON lost exact operands or availability")
	}
}

func timingFuzzValues(values []int64, known byte) []byte {
	raw := make([]byte, len(values)*9)
	for i, value := range values {
		raw[i*9] = known
		binary.LittleEndian.PutUint64(raw[i*9+1:], uint64(value))
	}
	return raw
}

func FuzzUserTimingExactStatistics(f *testing.F) {
	for _, values := range [][]int64{nil, {0}, {1, -1}, {math.MinInt64, math.MaxInt64, math.MaxInt64 - 2},
		{math.MaxInt64, math.MaxInt64}, {math.MinInt64, math.MinInt64}, {0, 0, 1},
		{50, 150, 250, -50, -150, -250}, {(1 << 53) - 1, 1 << 53, (1 << 53) + 1}} {
		f.Add(timingFuzzValues(values, 1))
	}
	f.Add(timingFuzzValues([]int64{0, math.MaxInt64}, 0))
	f.Add(append(timingFuzzValues([]int64{0, 7}, 1), timingFuzzValues([]int64{-8}, 0)...))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 64*9 {
			raw = raw[:64*9]
		}
		defer timingFuzzTrace(t, raw)
		var target userTimingValues
		var oracle []int64
		count := len(raw) / 9
		for i := 0; i < count; i++ {
			value := int64(binary.LittleEndian.Uint64(raw[i*9+1:]))
			known := raw[i*9]&1 != 0
			target.observe(value, known)
			if known {
				oracle = append(oracle, value)
			}
		}
		timingFuzzDistribution(t, target.result("synthetic-clock-offset"), oracle, count, "synthetic-clock-offset")
	})
}

type timingFuzzObservation struct {
	value int64
	known bool
}
type timingFuzzRow struct {
	kind                    string
	skipped, paid, included bool
	values                  [3]timingFuzzObservation
}

func timingFuzzScalar(row map[string]any, field, flag string, value int64, mode byte) timingFuzzObservation {
	result := timingFuzzObservation{}
	switch mode & 7 {
	case 0: // Absent scalar and declaration.
	case 1:
		row[field] = nil
	case 2:
		row[field] = value
		result = timingFuzzObservation{value, value != 0}
	case 3:
		row[field], row[flag] = value, true
		result = timingFuzzObservation{value, true}
	case 4:
		row[field], row[flag] = value, false
		result.value = value
	case 5:
		row[field], row[flag] = value, nil
		result.value = value
	case 6:
		row[flag] = true
	case 7:
		row[field], row[flag] = nil, true
	}
	return result
}

func timingFuzzCohort(t *testing.T, got UserDistribution, rows []timingFuzzRow, role int) {
	t.Helper()
	bases := []string{"receipt-observation-elapsed", "submit-to-block-clock-offset", "page-request-elapsed", "order-request-elapsed", "payout-anchor-to-observation-clock-offset"}
	var values []int64
	expected := 0
	for _, row := range rows {
		eligible := []bool{row.kind == "tx" && row.included, row.kind == "tx" && row.included,
			row.kind == "read", row.kind == "order", row.kind == "payout" && row.paid}[role]
		if row.skipped || !eligible {
			continue
		}
		expected++
		field := []int{0, 1, 2, 2, 0}[role]
		if row.values[field].known {
			values = append(values, row.values[field].value)
		}
	}
	timingFuzzDistribution(t, got, values, expected, bases[role])
}

func timingFuzzWireSeed(flags byte, mode uint16, confirm, include, latency int64) []byte {
	raw := make([]byte, 27)
	raw[0] = flags
	binary.LittleEndian.PutUint16(raw[1:], mode)
	for i, value := range []int64{confirm, include, latency} {
		binary.LittleEndian.PutUint64(raw[3+i*8:], uint64(value))
	}
	return raw
}

func FuzzUserTimingWireCohorts(f *testing.F) {
	f.Add([]byte{})
	var complete []byte
	for kind := byte(0); kind < 4; kind++ {
		for mode := uint16(0); mode < 8; mode++ {
			flags := kind | 4 | 16
			if mode&1 != 0 {
				flags |= 64
			}
			raw := timingFuzzWireSeed(flags, mode|mode<<3|mode<<6, 0, -500, 0)
			f.Add(raw)
			complete = append(complete, raw...)
		}
	}
	// Mixed retained roles, explicit unknowns, skips, unmined txs, reverted txs,
	// declared unknown payout anchors and wide values appear in normal tests.
	f.Add(complete[:16*27])
	f.Add(append(timingFuzzWireSeed(0|16|32, 3|3<<3|3<<6, math.MaxInt64, math.MinInt64, 0),
		timingFuzzWireSeed(3|4|32, 3|3<<3|3<<6, math.MinInt64, 0, math.MaxInt64)...))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 16*27 {
			raw = raw[:16*27]
		}
		defer timingFuzzTrace(t, raw)
		var total UsersTotals
		profile := &Agg{Name: "light"}
		actions := map[string]*Agg{}
		actionRows := map[string][]timingFuzzRow{}
		windows := [2]Window{}
		windowRows := [2][]timingFuzzRow{}
		var oracle []timingFuzzRow
		fields, flags := []string{"confirmMs", "includeMs", "latencyMs"}, []string{"confirmKnown", "includeKnown", "latencyKnown"}
		for i := 0; i+27 <= len(raw); i += 27 {
			header := raw[i]
			mode := binary.LittleEndian.Uint16(raw[i+1:])
			kind := []string{"tx", "read", "order", "payout"}[header&3]
			row := map[string]any{"t": int64(1000) + (int64(header>>6)&1)*1800000,
				"user": 0, "profile": "light", "action": "synthetic-timing-fuzz", "kind": kind, "ok": header&4 != 0}
			model := timingFuzzRow{kind: kind, skipped: header&8 != 0, paid: header&4 != 0, included: header&16 != 0}
			if model.skipped {
				row["skipped"] = "synthetic skip"
			}
			if model.included {
				row["block"] = 1
			}
			if kind == "tx" && !model.paid && header&32 != 0 {
				row["reverted"] = true
			}
			for field := 0; field < 3; field++ {
				value := int64(binary.LittleEndian.Uint64(raw[i+3+field*8:]))
				if field == 2 || field == 0 && kind != "payout" || field == 1 && kind != "tx" {
					value &= math.MaxInt64
				}
				model.values[field] = timingFuzzScalar(row, fields[field], flags[field], value, byte(mode>>uint(field*3)))
			}
			if kind == "payout" && header&32 != 0 {
				row["timestampBasis"], row["payoutAnchorKnown"] = "payout-observation-start", model.included
				if model.included {
					row["payoutAnchorMs"] = 0
				} else {
					model.values[0].known = false
				}
			}
			encoded, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			record, err := userRecord(encoded)
			if err != nil {
				t.Fatal("valid synthetic timing record was refused", err)
			}
			wire, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := userRecord(wire)
			if err != nil {
				t.Fatal("synthetic timing replay was refused", err)
			}
			for _, decoded := range []*Record{record, replay} {
				gotValues := []int64{decoded.ConfirmMS, decoded.IncludeMS, decoded.LatencyMS}
				gotKnown := []*bool{decoded.ConfirmKnown, decoded.IncludeKnown, decoded.LatencyKnown}
				for field := 0; field < 3; field++ {
					if gotValues[field] != model.values[field].value || gotKnown[field] == nil || *gotKnown[field] != model.values[field].known {
						t.Fatal("wire capture/replay invented or erased timing availability")
					}
				}
			}
			if i == 0 {
				row[strings.ToUpper(fields[header%3])] = 0
				alias, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := userRecord(alias); err == nil {
					t.Fatal("noncanonical timing alias was accepted")
				}
			}
			total.timings.observe(*record)
			observeUserAggregate(profile, *record, true)
			if actions[kind] == nil {
				actions[kind] = &Agg{Kind: kind, Name: record.Action}
			}
			observeUserAggregate(actions[kind], *record, false)
			oracle = append(oracle, model)
			actionRows[kind] = append(actionRows[kind], model)
			if kind != "payout" {
				bucket := int(header>>6) & 1
				windows[bucket].timings.observe(*record)
				windowRows[bucket] = append(windowRows[bucket], model)
			}
		}
		finishUserTimingTotals(&total)
		finishUserTimingAggregate(profile)
		for role, value := range []UserDistribution{total.Confirm, total.Include, total.Read, total.Order, total.PayoutDelay} {
			timingFuzzCohort(t, value, oracle, role)
		}
		for role, value := range []UserDistribution{profile.Confirm, profile.Include, profile.Latency, profile.Order, profile.PayoutDelay} {
			timingFuzzCohort(t, value, oracle, role)
		}
		for kind, action := range actions {
			finishUserTimingAggregate(action)
			rows := actionRows[kind]
			for role, value := range []UserDistribution{action.Confirm, action.Include, action.timings.read.result("page-request-elapsed"), action.Order, action.PayoutDelay} {
				timingFuzzCohort(t, value, rows, role)
			}
			switch kind {
			case "read":
				timingFuzzCohort(t, action.Latency, rows, 2)
			case "order":
				timingFuzzCohort(t, action.Latency, rows, 3)
			default:
				timingFuzzDistribution(t, action.Latency, nil, 0, "not-applicable")
			}
		}
		for i := range windows {
			finishUserTimingWindow(&windows[i])
			for role, value := range []UserDistribution{windows[i].Confirm, windows[i].Include, windows[i].Read, windows[i].Order} {
				timingFuzzCohort(t, value, windowRows[i], role)
			}
		}
	})
}

func FuzzUserTimingClockArithmetic(f *testing.F) {
	for _, seed := range []struct {
		after, before, seconds int64
		known                  bool
		rank                   uint64
	}{
		{0, 0, 0, true, 0}, {math.MaxInt64, math.MinInt64, math.MaxInt64, true, math.MaxUint64},
		{math.MinInt64, math.MaxInt64, -1, true, 1}, {100, 500, 1, true, 100},
		{math.MinInt64, 0, math.MaxInt64 / 1000, true, math.MaxUint64 - 1},
		{math.MaxInt64, 0, math.MaxInt64/1000 + 1, true, 1 << 63}, {0, math.MaxInt64, 0, false, 1000},
	} {
		f.Add(seed.after, seed.before, seed.seconds, seed.known, seed.rank)
	}
	f.Fuzz(func(t *testing.T, after, before, seconds int64, known bool, rank uint64) {
		var reproduction [33]byte
		for i, value := range []uint64{uint64(after), uint64(before), uint64(seconds), rank} {
			binary.LittleEndian.PutUint64(reproduction[i*8:], value)
		}
		if known {
			reproduction[32] = 1
		}
		defer timingFuzzTrace(t, reproduction[:])
		difference := new(big.Int).Sub(big.NewInt(after), big.NewInt(before))
		got, valid := userClockDifference(after, before)
		if valid != difference.IsInt64() || valid && got != difference.Int64() || !valid && got != 0 {
			t.Fatal("clock subtraction differs from unbounded integer oracle")
		}
		anchor := new(big.Int).Mul(big.NewInt(seconds), big.NewInt(1000))
		anchorKnown := known && seconds >= 0 && anchor.IsInt64()
		value, valid := userBlockMilliseconds(seconds, known)
		if valid != anchorKnown || valid && value != anchor.Int64() || !valid && value != 0 {
			t.Fatal("block timestamp narrowing differs")
		}
		payout := userPayoutRecord(1000, seconds, known)
		observeUserPayoutDelay(payout, after)
		delay := new(big.Int).Sub(big.NewInt(after), anchor)
		delayKnown := anchorKnown && delay.IsInt64()
		if payout.ConfirmKnown == nil || *payout.ConfirmKnown != delayKnown || delayKnown && payout.ConfirmMS != delay.Int64() {
			t.Fatal("payout offset availability differs")
		}
		maxInt := int(^uint(0) >> 1)
		n := int(rank%uint64(maxInt)) + 1
		for _, ratio := range [][2]int{{1, 2}, {9, 10}, {99, 100}} {
			want := new(big.Int).Quo(new(big.Int).Mul(big.NewInt(int64(n-1)), big.NewInt(int64(ratio[0]))), big.NewInt(int64(ratio[1])))
			if int64(userTimingRank(n, ratio[0], ratio[1])) != want.Int64() {
				t.Fatal("wide quantile rank differs")
			}
		}
	})
}
