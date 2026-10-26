package fleet

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestMeasurementCounterRepresentations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  uint64
		valid bool
	}{
		{"uint64", uint64(math.MaxUint64), math.MaxUint64, true},
		{"int64", int64(12), 12, true}, {"negative-int64", int64(-1), 0, false},
		{"int", int(12), 12, true}, {"negative-int", int(-1), 0, false},
		{"safe-legacy-float", float64(1<<53 - 1), 1<<53 - 1, true},
		{"unsafe-legacy-float", float64(1 << 53), 0, false},
		{"fractional-float", 1.5, 0, false}, {"infinity", math.Inf(1), 0, false},
		{"not-a-number", math.NaN(), 0, false}, {"negative-float", float64(-1), 0, false},
		{"decimal-string", "00012", 12, true}, {"signed-string", "+1", 0, false},
		{"overflow-string", "18446744073709551616", 0, false}, {"fractional-string", "1.0", 0, false},
		{"exponent", json.Number("1.25e2"), 125, true},
		{"full-width-exponent", json.Number("184467440737095516150e-1"), math.MaxUint64, true},
		{"negative-zero", json.Number("-0.0e99"), 0, true},
		{"huge-zero-exponent", json.Number("0e-999999999999999999999"), 0, true},
		{"huge-nonzero-exponent", json.Number("1e-999999999999999999999"), 0, false},
		{"overlong-scalar", json.Number(strings.Repeat("0", 129)), 0, false},
		{"nil", nil, 0, false}, {"boolean", true, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, valid := counterValue(tc.value)
			if got != tc.want || valid != tc.valid {
				t.Fatalf("counter = %d/%v, want %d/%v", got, valid, tc.want, tc.valid)
			}
		})
	}
}

func TestMeasurementSampleRoundTrip(t *testing.T) {
	sample := Sample{T: 1000, Relayer: map[string]any{"queued": json.Number("18446744073709551615")}}
	raw, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Sample
	if err := decodeMeasurementJSON(bytes.NewReader(raw), &decoded); err != nil {
		t.Fatal(err)
	}
	if num(decoded.Relayer["queued"]) != math.MaxUint64 {
		t.Fatal("sample counter lost precision during persistence")
	}
}

func TestMeasurementTypedJSONFields(t *testing.T) {
	var out struct {
		Value float64 `json:"value"`
		Count uint64  `json:"count"`
	}
	if err := decodeMeasurementJSON(strings.NewReader(`{"value":1.25,"count":18446744073709551615} `), &out); err != nil {
		t.Fatal(err)
	}
	if out.Value != 1.25 || out.Count != math.MaxUint64 {
		t.Fatal("typed JSON fields changed meaning")
	}
}

func TestMeasurementBinaryDisplayRounding(t *testing.T) {
	for _, tc := range []struct {
		value int64
		want  string
	}{
		{1280, "1.2 KB"}, {1792, "1.8 KB"}, {-1280, "-1.2 KB"},
		{math.MaxInt64, "8589934592.00 GB"}, {math.MinInt64, "-8589934592.00 GB"},
	} {
		if got := signedBytes(tc.value); got != tc.want {
			t.Fatalf("byte display = %q, want %q", got, tc.want)
		}
	}
}
