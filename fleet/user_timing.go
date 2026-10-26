package fleet

import (
	"fmt"
	"io"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

const userPayoutObservationBasis = "payout-observation-start"

// UserDistribution describes usable observations within a retained eligible
// cohort. Complete is coverage of that cohort, not proof of a complete run.
type UserDistribution struct {
	Dist
	Known                 bool   `json:"known"`
	Complete              bool   `json:"complete"`
	Expected              int    `json:"expected"`
	Basis                 string `json:"basis"`
	QuantilePolicy        string `json:"quantilePolicy"`
	MinExact              string `json:"minExact,omitempty"`
	P50Exact              string `json:"p50Exact,omitempty"`
	P90Exact              string `json:"p90Exact,omitempty"`
	P99Exact              string `json:"p99Exact,omitempty"`
	MaxExact              string `json:"maxExact,omitempty"`
	MeanNumerator         string `json:"meanNumerator,omitempty"`
	MeanDenominator       int    `json:"meanDenominator,omitempty"`
	QuantilesFloat64Known bool   `json:"quantilesFloat64Known"`
	MeanFloat64Known      bool   `json:"meanFloat64Known"`
}

type userTimingValues struct {
	values   []int64
	expected int
}

func (v *userTimingValues) observe(value int64, known bool) {
	v.expected++
	if known {
		v.values = append(v.values, value)
	}
}

func recordTimingKnown(value int64, known *bool) bool {
	if known != nil {
		return *known
	}
	return value != 0
}

func userTimingRank(n, num, den int) int {
	n--
	return n/den*num + n%den*num/den
}

func (v *userTimingValues) result(basis string) UserDistribution {
	d := UserDistribution{Dist: Dist{N: len(v.values)}, Expected: v.expected, Basis: basis, QuantilePolicy: "lower-index-floor((n-1)*p)"}
	if len(v.values) == 0 {
		return d
	}
	// These vectors belong to this reduction; sort in place rather than make
	// another full copy of each total/profile/action/window distribution.
	sort.Slice(v.values, func(i, j int) bool { return v.values[i] < v.values[j] })
	n := len(v.values)
	x := []int64{v.values[0], v.values[userTimingRank(n, 1, 2)], v.values[userTimingRank(n, 9, 10)], v.values[userTimingRank(n, 99, 100)], v.values[n-1]}
	d.Known, d.Complete = true, n == v.expected
	d.MinExact, d.P50Exact, d.P90Exact, d.P99Exact, d.MaxExact = strconv.FormatInt(x[0], 10), strconv.FormatInt(x[1], 10), strconv.FormatInt(x[2], 10), strconv.FormatInt(x[3], 10), strconv.FormatInt(x[4], 10)
	d.QuantilesFloat64Known = true
	floats := make([]float64, len(x))
	for i, value := range x {
		floats[i] = float64(value)
		if new(big.Rat).SetFloat64(floats[i]).Cmp(new(big.Rat).SetInt64(value)) != 0 {
			d.QuantilesFloat64Known = false
		}
	}
	if d.QuantilesFloat64Known {
		d.P50, d.P90, d.P99, d.Max = floats[1], floats[2], floats[3], floats[4]
	}
	var sum, addend big.Int
	for _, value := range v.values {
		addend.SetInt64(value)
		sum.Add(&sum, &addend)
	}
	d.MeanNumerator, d.MeanDenominator = sum.String(), n
	mean, exact := new(big.Rat).SetFrac(&sum, big.NewInt(int64(n))).Float64()
	d.MeanFloat64Known = exact
	if exact {
		d.Mean = mean
	}
	return d
}

type userTimings struct{ confirm, include, read, order, payout userTimingValues }

func (v *userTimings) observe(r Record) {
	if r.Skipped != "" {
		return
	}
	switch r.Kind {
	case "tx":
		if r.Block == 0 {
			return
		}
		v.confirm.observe(r.ConfirmMS, recordTimingKnown(r.ConfirmMS, r.ConfirmKnown))
		v.include.observe(r.IncludeMS, recordTimingKnown(r.IncludeMS, r.IncludeKnown))
	case "read":
		v.read.observe(r.LatencyMS, recordTimingKnown(r.LatencyMS, r.LatencyKnown))
	case "order":
		v.order.observe(r.LatencyMS, recordTimingKnown(r.LatencyMS, r.LatencyKnown))
	case "payout":
		if r.OK {
			v.payout.observe(r.ConfirmMS, recordTimingKnown(r.ConfirmMS, r.ConfirmKnown))
		}
	}
}

func finishUserTimingTotals(t *UsersTotals) {
	t.Confirm = t.timings.confirm.result("receipt-observation-elapsed")
	t.Include = t.timings.include.result("submit-to-block-clock-offset")
	t.Read = t.timings.read.result("page-request-elapsed")
	t.Order = t.timings.order.result("order-request-elapsed")
	t.PayoutDelay = t.timings.payout.result("payout-anchor-to-observation-clock-offset")
}

func finishUserTimingAggregate(a *Agg) {
	a.Confirm = a.timings.confirm.result("receipt-observation-elapsed")
	a.Include = a.timings.include.result("submit-to-block-clock-offset")
	a.Order = a.timings.order.result("order-request-elapsed")
	a.PayoutDelay = a.timings.payout.result("payout-anchor-to-observation-clock-offset")
	switch a.Kind {
	case "order":
		a.Latency = a.Order
	case "", "read":
		a.Latency = a.timings.read.result("page-request-elapsed")
	default:
		a.Latency = (&userTimingValues{}).result("not-applicable")
	}
}

func finishUserTimingWindow(w *Window) {
	w.Confirm = w.timings.confirm.result("receipt-observation-elapsed")
	w.Include = w.timings.include.result("submit-to-block-clock-offset")
	w.Read = w.timings.read.result("page-request-elapsed")
	w.Order = w.timings.order.result("order-request-elapsed")
}

func userTimingExact(d UserDistribution, key string) string {
	if !d.Known {
		return ""
	}
	switch key {
	case "min":
		return d.MinExact
	case "p50":
		return d.P50Exact
	case "p90":
		return d.P90Exact
	case "p99":
		return d.P99Exact
	case "max":
		return d.MaxExact
	}
	return ""
}

// Exact rational formatting, with nearest-even rounding and an explicit
// distinction between measured zero and an unavailable distribution.
func userTimingNumber(text string, denominator int64, decimals int) string {
	v, ok := new(big.Int).SetString(text, 10)
	if !ok || denominator <= 0 {
		return "n/a"
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	abs := new(big.Int).Abs(v)
	q, r := new(big.Int), new(big.Int)
	divisor := big.NewInt(denominator)
	q.QuoRem(new(big.Int).Mul(abs, scale), divisor, r)
	if cmp := new(big.Int).Lsh(r, 1).Cmp(divisor); cmp > 0 || cmp == 0 && q.Bit(0) != 0 {
		q.Add(q, big.NewInt(1))
	}
	out := q.String()
	if decimals > 0 {
		if len(out) <= decimals {
			out = strings.Repeat("0", decimals+1-len(out)) + out
		}
		out = out[:len(out)-decimals] + "." + out[len(out)-decimals:]
	}
	if v.Sign() < 0 && q.Sign() != 0 {
		out = "-" + out
	}
	return out
}

func userTimingMS(d UserDistribution, key string) string {
	return userTimingNumber(userTimingExact(d, key), 1, 0)
}
func userTimingSecs(d UserDistribution, key string) string {
	return userTimingNumber(userTimingExact(d, key), 1000, 1)
}

func writeUserTimingObservations(out io.Writer, s *UsersSummary) {
	fmt.Fprint(out, "\n## Timing observations\n\nValues describe usable samples within retained eligible rows. Coverage does not establish a complete run. Receipt confirmation uses included transaction rows, including reverts. Page and order requests include failures; skipped work is excluded. Block/payout clock offsets can be signed and are separate from elapsed client durations. Quantiles use the lower index floor((n-1)*p), without interpolation. Exact decimal quantiles and rational mean operands remain available when compatibility floats are unrepresentable.\n\n| Scope | Basis | Usable | Eligible | Complete coverage | Mean numerator, ms | Mean denominator | p50 ms | p90 ms | p99 ms | Min ms | Max ms |\n|---|---|---:|---:|---|---:|---:|---:|---:|---:|---:|---:|\n")
	row := func(scope string, d UserDistribution) {
		if d.Basis == "not-applicable" || d.Expected == 0 {
			return
		}
		mean, den := "n/a", "n/a"
		if d.Known {
			mean, den = d.MeanNumerator, strconv.Itoa(d.MeanDenominator)
		}
		scope = strings.NewReplacer("|", "\\|", "\n", " ", "\r", " ").Replace(scope)
		fmt.Fprintf(out, "| %s | %s | %d | %d | %t | %s | %s | %s | %s | %s | %s | %s |\n", scope, d.Basis, d.N, d.Expected, d.Complete, mean, den, userTimingMS(d, "p50"), userTimingMS(d, "p90"), userTimingMS(d, "p99"), userTimingMS(d, "min"), userTimingMS(d, "max"))
	}
	for _, d := range []UserDistribution{s.Totals.Confirm, s.Totals.Include, s.Totals.Read, s.Totals.Order, s.Totals.PayoutDelay} {
		row("total", d)
	}
	for _, a := range s.Profiles {
		for _, d := range []UserDistribution{a.Confirm, a.Include, a.Latency, a.Order, a.PayoutDelay} {
			row("profile "+a.Name, d)
		}
	}
	for _, a := range s.Actions {
		for _, d := range []UserDistribution{a.Confirm, a.Include, a.Latency, a.PayoutDelay} {
			row("action "+a.Kind+":"+a.Name, d)
		}
	}
	for _, w := range s.Windows {
		for _, d := range []UserDistribution{w.Confirm, w.Include, w.Read, w.Order} {
			row("window "+w.Start.UTC().Format("2006-01-02T15:04:05Z"), d)
		}
	}
}

func userClockDifference(after, before int64) (int64, bool) {
	v := new(big.Int).Sub(big.NewInt(after), big.NewInt(before))
	if !v.IsInt64() {
		return 0, false
	}
	return v.Int64(), true
}

func userBlockMilliseconds(seconds int64, known bool) (int64, bool) {
	if !known || seconds < 0 {
		return 0, false
	}
	v := new(big.Int).Mul(big.NewInt(seconds), big.NewInt(1000))
	if !v.IsInt64() {
		return 0, false
	}
	return v.Int64(), true
}

func userPayoutRecord(now, seconds int64, known bool) *Record {
	anchor, known := userBlockMilliseconds(seconds, known)
	unknown := false
	return &Record{T: now, TimestampBasis: userPayoutObservationBasis, PayoutAnchorMS: anchor, PayoutAnchorKnown: &known, ConfirmKnown: &unknown}
}

func observeUserPayoutDelay(r *Record, paidAt int64) {
	known := r.PayoutAnchorKnown != nil && *r.PayoutAnchorKnown
	if known {
		r.ConfirmMS, known = userClockDifference(paidAt, r.PayoutAnchorMS)
	}
	r.ConfirmKnown = &known
}
