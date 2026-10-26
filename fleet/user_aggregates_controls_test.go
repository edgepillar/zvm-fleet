package fleet

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUserAggregateRecordGasZeroSurvivesCaptureReplay(t *testing.T) {
	known := true
	raw, err := json.Marshal(Record{T: 1000, Profile: "light", Kind: "tx", Action: "synthetic", Block: 1, GasKnown: &known})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"gas":0`) {
		t.Fatal("measured zero was omitted during record capture")
	}
	var record Record
	if json.Unmarshal(raw, &record) != nil || record.GasKnown == nil || !*record.GasKnown {
		t.Fatal("measured gas zero lost its replay availability")
	}
	for _, text := range []string{`{"block":1,"gasKnown":true}`, `{"block":1,"gas":null,"gasKnown":true}`, `{"block":1,"gas":42,"gasKnown":false}`} {
		if json.Unmarshal([]byte(text), &record) != nil || recordGasKnown(record) {
			t.Fatal("declared availability invented an absent scalar or overrode false")
		}
	}
	raw, err = json.Marshal(Record{T: 1000, Kind: "read"})
	if err != nil || strings.Contains(string(raw), `"gas":`) {
		t.Fatal("uninvolved read records acquired an unused gas scalar")
	}
}

func TestUserAggregateSnapshotAvailabilitySurvivesReplay(t *testing.T) {
	for _, tc := range []struct {
		raw         string
		file, pages bool
	}{
		{`{"index":{"fileBytes":0,"pages":0,"freePages":0,"pageSize":4096},"indexFileBytesKnown":true}`, true, true},
		{`{"index":{"fileBytes":0,"pages":0,"freePages":0,"pageSize":4096}}`, false, true},
		{`{"index":{"fileBytes":9,"pages":1,"pageSize":4096},"indexPagesKnown":true}`, true, false},
		{`{"index":{"fileBytes":9,"pages":1,"freePages":0,"pageSize":4096},"indexPagesKnown":false}`, true, false},
	} {
		var s Snapshot
		if err := json.Unmarshal([]byte(tc.raw), &s); err != nil {
			t.Fatal(err)
		}
		if (snapshotFileBytes(&s) != nil) != tc.file || (snapshotIndex(&s, false) != nil) != tc.pages {
			t.Fatal("snapshot availability disagreed with field presence or explicit flags")
		}
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var again Snapshot
		if json.Unmarshal(raw, &again) != nil || (snapshotFileBytes(&again) != nil) != tc.file || (snapshotIndex(&again, false) != nil) != tc.pages {
			t.Fatal("snapshot replay changed availability")
		}
	}
	var s Snapshot
	if err := json.Unmarshal([]byte(`{"ops":{"derivation":{"totals":{"txs":18446744073709551615}}}}`), &s); err != nil {
		t.Fatal(err)
	}
	if got := snapshotTotal(&s, "txs"); got == nil || got.String() != "18446744073709551615" {
		t.Fatal("snapshot availability decoding coerced an unrelated full-width counter")
	}
}

func TestUserAggregateStorageRatiosUseExactNearestEven(t *testing.T) {
	for _, tc := range []struct {
		value       int64
		denominator int
		want        string
	}{
		{1, 2, "0 B"}, {3, 2, "2 B"}, {5, 2, "2 B"}, {-1, 2, "0 B"}, {-3, 2, "-2 B"}, {-5, 2, "-2 B"},
		{1024, 1, "1.0 KB"}, {2047, 2, "1024 B"}, {1024, 0, "n/a"},
	} {
		if got := userRatioBytes(big.NewInt(tc.value), tc.denominator); got != tc.want {
			t.Fatalf("ratio %d/%d = %s, want %s", tc.value, tc.denominator, got, tc.want)
		}
	}
}

func TestUserAggregateStorageRoundtripKeepsOperandsAndDenominator(t *testing.T) {
	p := &PhaseRecord{Before: &Snapshot{Dirs: map[string]DirSize{"synthetic:/data": {Bytes: 100}}}, After: &Snapshot{Dirs: map[string]DirSize{"synthetic:/data": {Bytes: 1}}}}
	s, _ := aggregateFixture(t, []map[string]any{aggregateTx(1000, 1), aggregateTx(2000, 1), aggregateTx(3000, 1)}, p)
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var again UsersSummary
	if json.Unmarshal(raw, &again) != nil || !reflect.DeepEqual(s.Storage, again.Storage) || again.Phase != nil {
		t.Fatal("summary did not retain independent exact storage operands")
	}
	cells := aggregateStorageCells(t, UsersMarkdown(&again), "synthetic:/data")
	if cells[3] != "-99 B" || cells[4] != "-33 B" || cells[5] != "3" {
		t.Fatal("roundtrip storage lost signed change or its real denominator")
	}
}

func TestUserAggregateIncompleteGasKeepsObservedSubset(t *testing.T) {
	missing := aggregateTx(2000, 0)
	delete(missing, "gas")
	rows := []map[string]any{aggregateTx(1000, 10), missing}
	s, _ := aggregateFixture(t, rows, nil)
	if s.Totals.GasTotalKnown || s.Totals.GasTotalUint64Known || s.Totals.GasObservedExact != "10" || s.Totals.GasSamples != 1 || s.Totals.GasExpected != 2 {
		t.Fatal("incomplete gas coverage became a complete total")
	}
	if s.Actions[0].GasMeanKnown || s.Actions[0].GasObservedExact != "10" || userGasMean(s.Actions[0]) != "n/a" {
		t.Fatal("incomplete gas coverage became a measured mean")
	}
}

func TestUserAggregateGasRationalAndRepresentabilityRemainExact(t *testing.T) {
	s, _ := aggregateFixture(t, []map[string]any{aggregateTx(1000, 1), aggregateTx(2000, 2), aggregateTx(3000, 2)}, nil)
	a := s.Actions[0]
	if !a.GasMeanKnown || a.GasMeanFloat64Known || a.GasMeanNumerator != "5" || a.GasMeanDenominator != 3 || userGasMean(a) != "1" {
		t.Fatal("non-binary rational mean lost its exact operands or floor display")
	}
	if !s.Totals.GasTotalKnown || !s.Totals.GasTotalUint64Known || s.Totals.GasTotal != 5 {
		t.Fatal("small exact total lost compatibility")
	}
	s, _ = aggregateFixture(t, []map[string]any{aggregateTx(1000, math.MaxUint64)}, nil)
	if s.Actions[0].GasMeanFloat64Known || userGasMean(s.Actions[0]) != "18,446,744,073,709,551,615" {
		t.Fatal("full-width mean was coerced to float or signed int")
	}
}

func TestUserAggregateGasSubsetUsesIncludedTransactions(t *testing.T) {
	failed := aggregateTx(2000, 999)
	failed["block"], failed["ok"] = 0, false
	order := aggregateTx(3000, 999)
	order["kind"], order["action"] = "order", "synthetic-order"
	skipped := aggregateTx(4000, 999)
	skipped["skipped"] = "synthetic skip"
	reverted := aggregateTx(5000, 3)
	reverted["ok"], reverted["reverted"] = false, true
	s, _ := aggregateFixture(t, []map[string]any{aggregateTx(1000, 2), failed, order, skipped, reverted}, nil)
	if !s.Totals.GasTotalKnown || s.Totals.GasExpected != 2 || s.Totals.GasSamples != 2 || s.Totals.GasTotalExact != "5" {
		t.Fatal("gas included nontransaction, unobserved or skipped records")
	}
	if s.Profiles[0].N != 3 || s.Profiles[0].Orders != 1 || s.Profiles[0].OK != 1 || s.Profiles[0].Reverted != 1 || s.Profiles[0].Failed != 1 {
		t.Fatal("profile transaction outcomes absorbed another event kind")
	}
}

func TestUserAggregateErrorTiesAreStableAndKindSpecific(t *testing.T) {
	for _, order := range [][]string{{"read", "payout", "order", "tx"}, {"tx", "order", "payout", "read"}, {"payout", "read", "tx", "order"}} {
		var rows []map[string]any
		for i, kind := range order {
			rows = append(rows, map[string]any{"t": 1000 + i*1000, "profile": "light", "kind": kind, "action": "synthetic-shared", "error": "synthetic failure"})
		}
		s, _ := aggregateFixture(t, rows, nil)
		if len(s.Errors) != 4 {
			t.Fatal("different error kinds were combined")
		}
		for i, kind := range []string{"tx", "order", "payout", "read"} {
			if s.Errors[i].Kind != kind || s.Errors[i].N != 1 {
				t.Fatal("equal-frequency errors changed order or lost their kind")
			}
		}
	}
}

func TestUserAggregateLogicalStorageCannotInventMissingFields(t *testing.T) {
	var s Snapshot
	if err := json.Unmarshal([]byte(`{"index":{"pages":1,"pageSize":4096},"indexPagesKnown":true,"followerDb":{"pages":1,"freePages":0,"pageSize":4096},"followerPagesKnown":false}`), &s); err != nil {
		t.Fatal(err)
	}
	if snapshotIndex(&s, false) != nil || snapshotIndex(&s, true) != nil {
		t.Fatal("missing or explicitly false page observations became usable")
	}
}

func TestUserAggregateStorageCaptureKeepsIndependentPresence(t *testing.T) {
	for _, tc := range []struct {
		raw         string
		file, pages bool
	}{
		{`{"fileBytes":0,"pages":0,"freePages":0,"pageSize":4096}`, true, true},
		{`{"pages":0,"freePages":0,"pageSize":4096}`, false, true},
		{`{"fileBytes":7,"pages":1,"pageSize":4096}`, true, false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			resourceDocker(t, tc.raw)
			e := settleFixtureEnv(t, settleFixtureOptions{})
			e.Names = Names{Node: "synthetic-node"}
			e.running = map[string]bool{"synthetic-node": true}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			s := e.snapshot(ctx)
			if s.IndexFileBytesKnown == nil || *s.IndexFileBytesKnown != tc.file || s.IndexPagesKnown == nil || *s.IndexPagesKnown != tc.pages {
				t.Fatal("capture fabricated or combined independent storage observations")
			}
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			var replay Snapshot
			if json.Unmarshal(raw, &replay) != nil || (snapshotFileBytes(&replay) != nil) != tc.file || (snapshotIndex(&replay, false) != nil) != tc.pages {
				t.Fatal("capture/replay changed independent storage availability")
			}
		})
	}
}

func TestUserAggregateAvailabilityAliasesCannotOverrideFlags(t *testing.T) {
	for _, raw := range []string{`{"gas":42,"GASKNOWN":false}`, `{"gas":42,"Gas":43,"gasKnown":true}`} {
		var record Record
		if json.Unmarshal([]byte(raw), &record) == nil {
			t.Fatal("noncanonical gas field supplied ambiguous availability")
		}
	}
	for _, raw := range []string{`{"index":{"fileBytes":42},"IndexFileBytesKnown":false}`, `{"index":{"pages":1,"freePages":0,"pageSize":4096},"INDEXPAGESKNOWN":false}`} {
		var snapshot Snapshot
		if json.Unmarshal([]byte(raw), &snapshot) == nil {
			t.Fatal("noncanonical storage flag supplied ambiguous availability")
		}
	}
}
