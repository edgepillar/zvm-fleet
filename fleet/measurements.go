package fleet

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

// decodeMeasurementJSON preserves number tokens in interface-valued metadata.
func decodeMeasurementJSON(reader io.Reader, out any) error {
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

// counterValue accepts exact nonnegative integers, never truncated fractions.
// Legacy float64 observations are usable only within the safe-integer range.
func counterValue(value any) (uint64, bool) {
	switch v := value.(type) {
	case json.Number:
		return counterToken(string(v))
	case string:
		if len(v) == 0 || len(v) > 128 {
			return 0, false
		}
		for _, digit := range v {
			if digit < '0' || digit > '9' {
				return 0, false
			}
		}
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	case uint64:
		return v, true
	case int64:
		if v >= 0 {
			return uint64(v), true
		}
	case int:
		if v >= 0 {
			return uint64(v), true
		}
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1<<53-1 || math.Trunc(v) != v {
			return 0, false
		}
		return uint64(v), true
	}
	return 0, false
}

// Parse decimal/exponent JSON integers using digit positions, with no large
// exponent allocation. The source scalar is bounded independently of its value.
func counterToken(token string) (uint64, bool) {
	if len(token) == 0 || len(token) > 128 || strings.TrimSpace(token) != token ||
		!json.Valid([]byte(token)) || (token[0] != '-' && (token[0] < '0' || token[0] > '9')) {
		return 0, false
	}
	negative := token[0] == '-'
	if negative {
		token = token[1:]
	}
	mantissa, exponent := token, "0"
	if i := strings.IndexAny(token, "eE"); i >= 0 {
		mantissa, exponent = token[:i], token[i+1:]
	}
	fraction := 0
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		fraction = len(mantissa) - i - 1
		mantissa = mantissa[:i] + mantissa[i+1:]
	}
	digits := strings.TrimLeft(mantissa, "0")
	if digits == "" {
		return 0, true
	}
	if negative {
		return 0, false
	}
	exp, err := strconv.ParseInt(exponent, 10, 32)
	if err != nil {
		return 0, false
	}
	shift := exp - int64(fraction)
	if shift < 0 {
		if -shift > int64(len(digits)) {
			return 0, false
		}
		keep := len(digits) - int(-shift)
		if strings.Trim(digits[keep:], "0") != "" {
			return 0, false
		}
		digits = digits[:keep]
	} else {
		if shift > 20 || int64(len(digits))+shift > 20 {
			return 0, false
		}
		digits += strings.Repeat("0", int(shift))
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

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
