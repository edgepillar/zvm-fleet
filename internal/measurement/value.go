package measurement

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// Decode preserves number tokens in interface-valued metadata.
func Decode(reader io.Reader, out any) error {
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

// Value accepts exact nonnegative integers, never truncated fractions.
// Legacy float64 observations are usable only within the safe-integer range.
func Value(value any) (uint64, bool) {
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
