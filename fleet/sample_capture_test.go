package fleet

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"
)

func TestSampleCaptureCounterAvailability(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  uint64
		known bool
	}{
		{"zero", json.Number("0"), 0, true},
		{"full-width", json.Number("18446744073709551615"), math.MaxUint64, true},
		{"decimal-string", "12", 12, true},
		{"exponent", json.Number("1.25e2"), 125, true},
		{"null", nil, 0, false},
		{"negative", json.Number("-1"), 0, false},
		{"fractional", json.Number("1.5"), 0, false},
		{"overflow", json.Number("18446744073709551616"), 0, false},
		{"boolean", true, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sample Sample
			sample.captureInfo(map[string]any{
				"l2": map[string]any{"head": tc.value}, "l1": map[string]any{"frontier": tc.value},
				"derivation": map[string]any{"momentum": tc.value},
			})
			if sample.Known == nil || sample.Known.L2 != tc.known || sample.Known.L1 != tc.known || sample.Known.Derived != tc.known {
				t.Fatal("numeric availability does not match the captured input")
			}
			if sample.L2 != tc.want || sample.L1 != tc.want || sample.Derived != tc.want {
				t.Fatal("captured counter value changed")
			}
			if sample.Known.Indexed || sample.Known.Follower {
				t.Fatal("info response claimed an unrelated probe observation")
			}
		})
	}
}

func TestSampleCaptureMissingCountersClearPriorObservations(t *testing.T) {
	var sample Sample
	sample.captureInfo(map[string]any{
		"l2": map[string]any{"head": uint64(1)}, "l1": map[string]any{"frontier": uint64(2)},
		"derivation": map[string]any{"momentum": uint64(3)},
		"relayer":    map[string]any{"queued": uint64(4), "accounts": []any{"synthetic-private-input"}},
		"bridge":     map[string]any{"queued": uint64(5)}, "faucet": map[string]any{"served": uint64(6)},
	})
	if _, leaked := sample.Relayer["accounts"]; leaked {
		t.Fatal("sample retained private relayer account data")
	}
	sample.captureInfo(map[string]any{})
	if sample.L2 != 0 || sample.L1 != 0 || sample.Derived != 0 || sample.Known.L2 || sample.Known.L1 || sample.Known.Derived {
		t.Fatal("missing counters retained an earlier observation")
	}
	if sample.Relayer != nil || sample.Bridge != nil || sample.Faucet != nil {
		t.Fatal("missing info retained an earlier service response")
	}
}

func TestSampleCaptureAvailabilityRoundTrip(t *testing.T) {
	sample := Sample{L2: 0, Indexed: math.MaxUint64, L1: 1, Derived: 2,
		Known: &SampleKnown{L2: true, Indexed: true, Follower: false, L1: true, Derived: true}}
	raw, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Sample
	if err := decodeMeasurementJSON(bytes.NewReader(raw), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Known == nil || *decoded.Known != *sample.Known || decoded.Indexed != math.MaxUint64 || decoded.L2 != 0 {
		t.Fatal("sample persistence lost zero/full-width values or observation availability")
	}
}

func TestSampleWindowIndependentCounterAvailability(t *testing.T) {
	for _, l2Known := range []bool{false, true} {
		rows := []map[string]any{sampleAccountingObservation(1, 10, nil), sampleAccountingObservation(2, 13, nil)}
		for _, row := range rows {
			row["known"] = map[string]bool{"l2": l2Known, "l1": !l2Known}
		}
		cells := sampleAccountingCells(t, sampleAccountingSummary(t, rows), "00:00")
		l2Want, l1Want := "n/a", "3"
		if l2Known {
			l2Want, l1Want = l1Want, l2Want
		}
		if cells[8] != l2Want || cells[9] != l1Want {
			t.Fatal("an unavailable counter changed an independently observed counter")
		}
	}
}
