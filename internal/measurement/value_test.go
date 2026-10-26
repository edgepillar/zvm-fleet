package measurement

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"testing"
)

// The independent oracle uses exact rationals, bounded before exponent parsing.
// Extreme nonzero exponents cannot produce a uint64 integer from a 128-byte token.
func rationalCounterOracle(text string) (uint64, bool) {
	if len(text) == 0 || len(text) > 128 || strings.TrimSpace(text) != text || !json.Valid([]byte(text)) ||
		(text[0] != '-' && (text[0] < '0' || text[0] > '9')) {
		return 0, false
	}
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		exponent, err := strconv.ParseInt(text[i+1:], 10, 64)
		if err != nil || exponent < -1024 || exponent > 1024 {
			return 0, strings.Trim(text[:i], "-0.") == ""
		}
	}
	rational, ok := new(big.Rat).SetString(text)
	if !ok || !rational.IsInt() || rational.Sign() < 0 || rational.Num().BitLen() > 64 {
		return 0, false
	}
	return rational.Num().Uint64(), true
}

func FuzzMeasurementCounter(f *testing.F) {
	for _, seed := range []string{"0", "-0.0", "1", "9007199254740993", "18446744073709551615", "184467440737095516150e-1",
		"18446744073709551616", "1.25e2", "1.5", "1e-2", "1e999999999", "0e-999999999999999999", "true", "{}", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		got, valid := Value(json.Number(text))
		want, expected := rationalCounterOracle(text)
		if valid != expected || (valid && got != want) {
			t.Fatalf("counter/oracle disagreement for synthetic token %x: %d/%v versus %d/%v", []byte(text), got, valid, want, expected)
		}
	})
}
