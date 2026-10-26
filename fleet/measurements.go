package fleet

import (
	"io"
	"math/big"
	"strconv"
	"strings"

	"github.com/Zenoglyphs-VM/zvm-fleet/internal/measurement"
	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

func decodeMeasurementJSON(reader io.Reader, out any) error { return measurement.Decode(reader, out) }

func counterValue(value any) (uint64, bool) { return measurement.Value(value) }

func counterDifference(before, after *big.Int) *big.Int {
	if before == nil || after == nil || after.Cmp(before) < 0 {
		return nil
	}
	return new(big.Int).Sub(after, before)
}

func signedDifference(before, after *big.Int) *big.Int {
	if before == nil || after == nil {
		return nil
	}
	return new(big.Int).Sub(after, before)
}

func snapshotTotal(s *Snapshot, key string) *big.Int {
	if s == nil {
		return nil
	}
	d, _ := s.Ops["derivation"].(map[string]any)
	t, _ := d["totals"].(map[string]any)
	n, ok := counterValue(t[key])
	if !ok {
		return nil
	}
	return new(big.Int).SetUint64(n)
}

func totalsDelta(p *PhaseRecord, key string) *big.Int {
	return counterDifference(snapshotTotal(p.Before, key), snapshotTotal(p.After, key))
}

func snapshotHead(s *Snapshot) *big.Int {
	if s == nil || (s.L2HeadKnown != nil && !*s.L2HeadKnown) || (s.L2HeadKnown == nil && s.L2Head == 0) {
		return nil
	}
	return new(big.Int).SetUint64(s.L2Head)
}

func directoryValue(s *Snapshot, key string, allocated bool) (*big.Int, bool) {
	if s == nil {
		return nil, false
	}
	d, present := s.Dirs[key]
	if !present {
		return nil, false
	}
	n := d.Bytes
	if allocated {
		n = d.Allocated
	}
	if n < 0 {
		return nil, true
	}
	return big.NewInt(n), true
}

func writeValue(s *Snapshot, key string) (*big.Int, bool) {
	if s == nil {
		return nil, false
	}
	c, present := s.Containers[key]
	if !present {
		return nil, false
	}
	if c.BlockWrite < 0 {
		return nil, true
	}
	return big.NewInt(c.BlockWrite), true
}

func sqliteValue(report *bench.StorageReport) *big.Int {
	if report == nil || report.Pages < 0 || report.FreePages < 0 || report.FreePages > report.Pages || report.PageSize <= 0 {
		return nil
	}
	return new(big.Int).Mul(big.NewInt(report.Pages-report.FreePages), big.NewInt(report.PageSize))
}

func snapshotIndex(s *Snapshot, follower bool) *big.Int {
	if s == nil {
		return nil
	}
	if follower {
		return sqliteValue(s.FollowerDB)
	}
	return sqliteValue(s.Index)
}

func measurementCount(value *big.Int) string {
	if value == nil {
		return "n/a"
	}
	return decimalGroups(value.String())
}

func measurementRawCount(value *big.Int) string {
	if value == nil {
		return "n/a"
	}
	return value.String()
}

func unsignedCommas(value uint64) string {
	return decimalGroups(strconv.FormatUint(value, 10))
}

func decimalGroups(value string) string {
	sign := ""
	if strings.HasPrefix(value, "-") {
		sign, value = "-", value[1:]
	}
	for i := len(value) - 3; i > 0; i -= 3 {
		value = value[:i] + "," + value[i:]
	}
	return sign + value
}

func measurementBytes(value *big.Int) string {
	if value == nil {
		return "n/a"
	}
	abs := new(big.Int).Abs(value)
	for _, unit := range []struct {
		size     int64
		decimals int
		label    string
	}{{1 << 30, 2, "GB"}, {1 << 20, 1, "MB"}, {1 << 10, 1, "KB"}} {
		if abs.Cmp(big.NewInt(unit.size)) >= 0 {
			return roundedMeasurement(value, unit.size, unit.decimals) + " " + unit.label
		}
	}
	return value.String() + " B"
}

// Match the existing nearest-even display rounding without float64 coercion.
func roundedMeasurement(value *big.Int, unit int64, decimals int) string {
	n := new(big.Int).Abs(value)
	scale := int64(1)
	for range decimals {
		scale *= 10
	}
	n.Mul(n, big.NewInt(scale))
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(n, big.NewInt(unit), r)
	cmp := r.Lsh(r, 1).Cmp(big.NewInt(unit))
	if cmp > 0 || (cmp == 0 && q.Bit(0) != 0) {
		q.Add(q, big.NewInt(1))
	}
	text := q.String()
	if decimals > 0 {
		if len(text) <= decimals {
			text = strings.Repeat("0", decimals+1-len(text)) + text
		}
		text = text[:len(text)-decimals] + "." + text[len(text)-decimals:]
	}
	if value.Sign() < 0 {
		text = "-" + text
	}
	return text
}
