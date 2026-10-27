package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

type recordAggregateJSON Record

// Keep successful zero gas explicit without adding a gas scalar to every read.
func (r Record) MarshalJSON() ([]byte, error) {
	var gas *uint64
	if r.GasUsed > 0 || r.GasKnown != nil && *r.GasKnown {
		gas = &r.GasUsed
	}
	return json.Marshal(struct {
		recordAggregateJSON
		Gas     *uint64 `json:"gas,omitempty"`
		Confirm *int64  `json:"confirmMs,omitempty"`
		Include *int64  `json:"includeMs,omitempty"`
		Latency *int64  `json:"latencyMs,omitempty"`
		Anchor  *int64  `json:"payoutAnchorMs,omitempty"`
	}{recordAggregateJSON(r), gas, timingScalar(r.ConfirmMS, r.ConfirmKnown), timingScalar(r.IncludeMS, r.IncludeKnown), timingScalar(r.LatencyMS, r.LatencyKnown), timingScalar(r.PayoutAnchorMS, r.PayoutAnchorKnown)})
}

func timingScalar(value int64, known *bool) *int64 {
	if value != 0 || known != nil && *known {
		return &value
	}
	return nil
}

func replayTimingKnown(fields map[string]json.RawMessage, scalar, flag string, value int64, declared *bool) *bool {
	known := presentScalar(fields, scalar) && value != 0
	if _, present := fields[flag]; present {
		known = presentScalar(fields, scalar) && declared != nil && *declared
	}
	return &known
}

func (r *Record) UnmarshalJSON(raw []byte) error {
	var value recordAggregateJSON
	if err := decodeMeasurementJSON(bytes.NewReader(raw), &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if err := canonicalAggregateFields(fields, "gas", "gasKnown", "confirmMs", "confirmKnown", "includeMs", "includeKnown", "latencyMs", "latencyKnown", "timestampBasis", "payoutAnchorMs", "payoutAnchorKnown"); err != nil {
		return err
	}
	present := presentScalar(fields, "gas")
	known := present && value.GasUsed > 0
	if _, declared := fields["gasKnown"]; declared {
		known = present && value.GasKnown != nil && *value.GasKnown
	}
	value.GasKnown = &known
	value.ConfirmKnown = replayTimingKnown(fields, "confirmMs", "confirmKnown", value.ConfirmMS, value.ConfirmKnown)
	value.IncludeKnown = replayTimingKnown(fields, "includeMs", "includeKnown", value.IncludeMS, value.IncludeKnown)
	value.LatencyKnown = replayTimingKnown(fields, "latencyMs", "latencyKnown", value.LatencyMS, value.LatencyKnown)
	value.PayoutAnchorKnown = replayTimingKnown(fields, "payoutAnchorMs", "payoutAnchorKnown", value.PayoutAnchorMS, value.PayoutAnchorKnown)
	if value.TimestampBasis == userPayoutObservationBasis && !*value.PayoutAnchorKnown {
		unknown := false
		value.ConfirmKnown = &unknown
	}
	*r = Record(value)
	return nil
}

func presentScalar(fields map[string]json.RawMessage, key string) bool {
	raw, present := fields[key]
	return present && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func canonicalAggregateFields(fields map[string]json.RawMessage, names ...string) error {
	for key := range fields {
		for _, name := range names {
			if key != name && strings.EqualFold(key, name) {
				return fmt.Errorf("noncanonical aggregate field %s", name)
			}
		}
	}
	return nil
}

func storageAvailability(raw []byte, report *bench.StorageReport) (file, pages bool, failure error) {
	if report == nil {
		return false, false, nil
	}
	// Typed decoding folds field names and accepts duplicate members. Presence
	// and availability must refer to the same unambiguous canonical fields.
	if err := validateUserObject(raw, len(raw)+1); err != nil {
		return false, false, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false, false, err
	}
	if err := canonicalAggregateFields(fields, "fileBytes", "pages", "freePages", "pageSize"); err != nil {
		return false, false, err
	}
	file = presentScalar(fields, "fileBytes") && report.FileBytes >= 0
	pages = presentScalar(fields, "pages") && presentScalar(fields, "freePages") && presentScalar(fields, "pageSize") && sqliteValue(report) != nil
	return file, pages, nil
}

type snapshotAggregateJSON Snapshot

func (s *Snapshot) UnmarshalJSON(raw []byte) error {
	if err := validateUserObject(raw, len(raw)+1); err != nil {
		return err
	}
	var value snapshotAggregateJSON
	if err := decodeMeasurementJSON(bytes.NewReader(raw), &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if err := canonicalAggregateFields(fields, "index", "followerDb", "indexFileBytesKnown", "indexPagesKnown", "followerPagesKnown"); err != nil {
		return err
	}
	file, pages, err := storageAvailability(fields["index"], value.Index)
	if err != nil {
		return fmt.Errorf("index observation: %w", err)
	}
	_, follower, err := storageAvailability(fields["followerDb"], value.FollowerDB)
	if err != nil {
		return fmt.Errorf("follower observation: %w", err)
	}
	// A legacy zero file size cannot distinguish an absent scalar from a
	// successful zero capture. New capture/replay flags preserve that distinction.
	if _, declared := fields["indexFileBytesKnown"]; declared {
		file = file && value.IndexFileBytesKnown != nil && *value.IndexFileBytesKnown
	} else {
		file = file && value.Index != nil && value.Index.FileBytes > 0
	}
	if _, declared := fields["indexPagesKnown"]; declared {
		pages = pages && value.IndexPagesKnown != nil && *value.IndexPagesKnown
	}
	if _, declared := fields["followerPagesKnown"]; declared {
		follower = follower && value.FollowerPagesKnown != nil && *value.FollowerPagesKnown
	}
	value.IndexFileBytesKnown, value.IndexPagesKnown, value.FollowerPagesKnown = &file, &pages, &follower
	*s = Snapshot(value)
	return nil
}

func snapshotFileBytes(s *Snapshot) *big.Int {
	if s == nil || s.Index == nil || s.Index.FileBytes < 0 ||
		s.IndexFileBytesKnown != nil && !*s.IndexFileBytesKnown || s.IndexFileBytesKnown == nil && s.Index.FileBytes == 0 {
		return nil
	}
	return big.NewInt(s.Index.FileBytes)
}

type UserStorageMetric struct {
	Name              string `json:"name"`
	Basis             string `json:"basis"`
	BeforeExact       string `json:"beforeExact,omitempty"`
	AfterExact        string `json:"afterExact,omitempty"`
	DeltaExact        string `json:"deltaExact,omitempty"`
	BeforeKnown       bool   `json:"beforeKnown"`
	AfterKnown        bool   `json:"afterKnown"`
	DeltaKnown        bool   `json:"deltaKnown"`
	ConfirmedRecords  int    `json:"confirmedRecords"`
	PerConfirmedKnown bool   `json:"perConfirmedKnown"`
}

func userStorageMetric(name, basis string, before, after *big.Int, confirmed int) UserStorageMetric {
	row := UserStorageMetric{Name: name, Basis: basis, ConfirmedRecords: confirmed}
	if before != nil {
		row.BeforeKnown, row.BeforeExact = true, before.String()
	}
	if after != nil {
		row.AfterKnown, row.AfterExact = true, after.String()
	}
	if delta := signedDifference(before, after); delta != nil {
		row.DeltaKnown, row.DeltaExact = true, delta.String()
		row.PerConfirmedKnown = confirmed > 0
	}
	return row
}

func userStorageMetrics(p *PhaseRecord, confirmed int) []UserStorageMetric {
	if p == nil {
		return nil
	}
	keys := map[string]bool{}
	for _, s := range []*Snapshot{p.Before, p.After} {
		if s != nil {
			for key := range s.Dirs {
				keys[key] = true
			}
		}
	}
	var rows []UserStorageMetric
	for _, key := range sortedKeys(keys) {
		before, _ := directoryValue(p.Before, key, false)
		after, _ := directoryValue(p.After, key, false)
		rows = append(rows, userStorageMetric(key, "directory-apparent-bytes", before, after, confirmed))
	}
	rows = append(rows, userStorageMetric("index (SQLite file)", "sqlite-file-bytes", snapshotFileBytes(p.Before), snapshotFileBytes(p.After), confirmed))
	rows = append(rows, userStorageMetric("index (SQLite pages in use)", "sqlite-pages-in-use", snapshotIndex(p.Before, false), snapshotIndex(p.After, false), confirmed))
	return rows
}

func userMetricInt(text string, known bool) *big.Int {
	if !known {
		return nil
	}
	value, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return nil
	}
	return value
}

func userRatioBytes(value *big.Int, denominator int) string {
	if value == nil || denominator <= 0 {
		return "n/a"
	}
	den := big.NewInt(int64(denominator))
	abs := new(big.Int).Abs(value)
	for _, unit := range []struct {
		size     int64
		decimals int
		label    string
	}{
		{1 << 30, 2, "GB"}, {1 << 20, 1, "MB"}, {1 << 10, 1, "KB"}, {1, 0, "B"},
	} {
		divisor := new(big.Int).Mul(den, big.NewInt(unit.size))
		if unit.size != 1 && abs.Cmp(divisor) < 0 {
			continue
		}
		scale := big.NewInt(1)
		for range unit.decimals {
			scale.Mul(scale, big.NewInt(10))
		}
		q, r := new(big.Int), new(big.Int)
		q.QuoRem(new(big.Int).Mul(abs, scale), divisor, r)
		cmp := new(big.Int).Lsh(r, 1).Cmp(divisor)
		if cmp > 0 || cmp == 0 && q.Bit(0) != 0 {
			q.Add(q, big.NewInt(1))
		}
		text := q.String()
		if unit.decimals > 0 {
			if len(text) <= unit.decimals {
				text = strings.Repeat("0", unit.decimals+1-len(text)) + text
			}
			text = text[:len(text)-unit.decimals] + "." + text[len(text)-unit.decimals:]
		}
		if value.Sign() < 0 && q.Sign() != 0 {
			text = "-" + text
		}
		return text + " " + unit.label
	}
	return "n/a"
}

func renderUserStorage(out *strings.Builder, rows []UserStorageMetric) {
	if len(rows) == 0 {
		return
	}
	out.WriteString("\n## Storage written over the run\n\nSigned changes between usable snapshot endpoints. Directory bytes are apparent size; SQLite file size and pages in use are separate. `n/a` preserves missing/invalid inputs. Per-record ratios use only retained confirmed transaction records, not causal attribution or complete-run proof. JSON retains exact decimal operands and the actual denominator. Display uses binary scaling and nearest-even rounding.\n\n| Directory | Before | After | Written | Per confirmed tx record | Confirmed records |\n|---|---:|---:|---:|---:|---:|\n")
	for _, row := range rows {
		before, after := userMetricInt(row.BeforeExact, row.BeforeKnown), userMetricInt(row.AfterExact, row.AfterKnown)
		delta := userMetricInt(row.DeltaExact, row.DeltaKnown)
		per := "n/a"
		if row.PerConfirmedKnown {
			per = userRatioBytes(delta, row.ConfirmedRecords)
		}
		fmt.Fprintf(out, "| %s | %s | %s | %s | %s | %d |\n", row.Name, measurementBytes(before), measurementBytes(after), measurementBytes(delta), per, row.ConfirmedRecords)
	}
}

func recordGasKnown(r Record) bool {
	if r.GasKnown != nil {
		return *r.GasKnown
	}
	return r.GasUsed > 0
}

func observeUserAggregate(a *Agg, r Record, profile bool) {
	a.timings.observe(r)
	if r.Skipped != "" {
		a.Skipped++
		return
	}
	if r.Kind == "read" {
		a.Reads++
		if !r.OK {
			a.ReadErrors++
		} else if !profile {
			a.OK++
		}
		return
	}
	if profile && r.Kind != "tx" {
		if r.Kind == "order" {
			a.Orders++
		}
		if r.Kind == "payout" {
			a.Payouts++
		}
		return
	}
	a.N++
	switch {
	case r.OK:
		a.OK++
	case r.Reverted:
		a.Reverted++
	default:
		a.Failed++
	}
	if r.Kind == "tx" && r.Block != 0 {
		a.GasExpected++
		if recordGasKnown(r) {
			a.GasSamples++
			a.gas.Add(&a.gas, new(big.Int).SetUint64(r.GasUsed))
		}
	}
}

func finishUserGas(a *Agg) {
	if a.GasSamples > 0 {
		a.GasObservedExact = a.gas.String()
	}
	a.GasMeanKnown = a.GasExpected > 0 && a.GasSamples == a.GasExpected
	if !a.GasMeanKnown {
		return
	}
	a.GasMeanNumerator, a.GasMeanDenominator = a.gas.String(), a.GasSamples
	ratio := new(big.Rat).SetFrac(&a.gas, big.NewInt(int64(a.GasSamples)))
	value, exact := ratio.Float64()
	a.GasMeanFloat64Known = exact
	if exact {
		a.GasMean = value
	}
}

func userGasMean(a *Agg) string {
	if !a.GasMeanKnown || a.GasMeanDenominator <= 0 {
		return "n/a"
	}
	numerator, ok := new(big.Int).SetString(a.GasMeanNumerator, 10)
	if !ok {
		return "n/a"
	}
	return measurementCount(numerator.Quo(numerator, big.NewInt(int64(a.GasMeanDenominator))))
}

func finishUserGasTotal(t *UsersTotals, sum *big.Int) {
	if t.GasSamples > 0 {
		t.GasObservedExact = sum.String()
	}
	t.GasTotalKnown = t.GasExpected > 0 && t.GasSamples == t.GasExpected
	if !t.GasTotalKnown {
		return
	}
	t.GasTotalExact = sum.String()
	t.GasTotalUint64Known = sum.IsUint64()
	if t.GasTotalUint64Known {
		t.GasTotal = sum.Uint64()
	}
}

func userGasTotal(t *UsersTotals) string {
	if !t.GasTotalKnown {
		return "n/a"
	}
	return measurementCount(userMetricInt(t.GasTotalExact, true))
}

func sortUserErrors(rows []ErrorCount) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.N != b.N {
			return a.N > b.N
		}
		if a.Kind != b.Kind {
			return kindOrder(a.Kind) < kindOrder(b.Kind)
		}
		if a.Action != b.Action {
			return a.Action < b.Action
		}
		return a.Error < b.Error
	})
}
