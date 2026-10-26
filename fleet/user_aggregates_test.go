package fleet

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

func aggregateStorageCells(t *testing.T, text, name string) []string {
	t.Helper()
	for _, row := range strings.Split(text, "\n") {
		if strings.HasPrefix(row, "| "+name+" |") {
			cells := strings.Split(row, "|")
			cells = cells[1 : len(cells)-1]
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}
			return cells
		}
	}
	t.Fatalf("expected storage row %q disappeared", name)
	return nil
}

func TestUserStorageRequiresUsableEndpoints(t *testing.T) {
	for _, name := range []string{"missing-before", "missing-after", "missing-snapshot", "unavailable", "negative"} {
		t.Run(name, func(t *testing.T) {
			p := &PhaseRecord{Before: &Snapshot{Dirs: map[string]DirSize{"synthetic:/data": {Bytes: 10}}},
				After: &Snapshot{Dirs: map[string]DirSize{"synthetic:/data": {Bytes: 20}}}}
			missing := 1
			switch name {
			case "missing-before":
				delete(p.Before.Dirs, "synthetic:/data")
			case "missing-after":
				delete(p.After.Dirs, "synthetic:/data")
				missing = 2
			case "missing-snapshot":
				p.Before = nil
			case "unavailable":
				p.Before.Dirs["synthetic:/data"] = DirSize{Bytes: 10, Known: map[string]bool{"bytes": false}}
			case "negative":
				p.Before.Dirs["synthetic:/data"] = DirSize{Bytes: -1}
			}
			cells := aggregateStorageCells(t, UsersMarkdown(&UsersSummary{Phase: p, Totals: UsersTotals{Confirmed: 1}}), "synthetic:/data")
			if len(cells) < 5 || cells[missing] != "n/a" || cells[3] != "n/a" || cells[4] != "n/a" {
				t.Fatal("unavailable storage endpoint became a measured zero or delta")
			}
		})
	}
}

func TestUserStorageDoesNotInventConfirmedDenominator(t *testing.T) {
	p := &PhaseRecord{Before: &Snapshot{Index: &bench.StorageReport{FileBytes: 10}}, After: &Snapshot{Index: &bench.StorageReport{FileBytes: 20}}}
	cells := aggregateStorageCells(t, UsersMarkdown(&UsersSummary{Phase: p}), "index (SQLite file)")
	if len(cells) < 5 || cells[4] != "n/a" {
		t.Fatal("zero confirmed records were replaced with a denominator of one")
	}
}

func TestUserStorageFilePresenceCannotBeInvented(t *testing.T) {
	for _, raw := range []string{
		`{"before":{"index":{"fileBytes":10},"indexFileBytesKnown":false},"after":{"index":{"fileBytes":20}}}`,
		`{"before":{"index":{},"indexFileBytesKnown":true},"after":{"index":{"fileBytes":20}}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var p PhaseRecord
			if err := decodeMeasurementJSON(strings.NewReader(raw), &p); err != nil {
				t.Fatal(err)
			}
			cells := aggregateStorageCells(t, UsersMarkdown(&UsersSummary{Phase: &p, Totals: UsersTotals{Confirmed: 1}}), "index (SQLite file)")
			if cells[1] != "n/a" || cells[3] != "n/a" {
				t.Fatal("absent or explicitly unknown file bytes became a usable endpoint")
			}
		})
	}
}

func aggregateFixture(t *testing.T, records []map[string]any, phase *PhaseRecord) (*UsersSummary, string) {
	t.Helper()
	dir := t.TempDir()
	writeSampleAccountingLines(t, filepath.Join(dir, "records.jsonl"), records)
	if phase != nil {
		phase.ID = "00-users-synthetic"
		contextWriteJSON(t, dir, "00-users-synthetic.json", phase)
	}
	s, err := SummarizeUsers(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func aggregateTx(at int64, gas uint64) map[string]any {
	return map[string]any{"t": at, "profile": "light", "kind": "tx", "action": "synthetic-tx", "ok": true, "block": at, "gas": gas}
}

func TestUserGasAggregatesAvoidFullWidthWrapping(t *testing.T) {
	s, _ := aggregateFixture(t, []map[string]any{aggregateTx(1000, math.MaxUint64), aggregateTx(2000, math.MaxUint64)}, nil)
	text := UsersMarkdown(s)
	if !strings.Contains(text, "| Gas used | 36,893,488,147,419,103,230 |") {
		t.Error("full-width gas sum wrapped or rendered as a signed number")
	}
	if !strings.Contains(text, "18,446,744,073,709,551,615") {
		t.Error("full-width gas mean lost exact integer display")
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Totals struct {
			Exact string `json:"gasTotalExact"`
			Known bool   `json:"gasTotalKnown"`
			Fits  bool   `json:"gasTotalUint64Known"`
		} `json:"totals"`
	}
	if json.Unmarshal(raw, &wire) != nil || wire.Totals.Exact != "36893488147419103230" || !wire.Totals.Known || wire.Totals.Fits {
		t.Error("summary omitted exact gas total or representability evidence")
	}
}

func TestUserGasCoverageDistinguishesMissingAndMeasuredZero(t *testing.T) {
	for _, measured := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "measured-zero"}[measured], func(t *testing.T) {
			record := aggregateTx(1000, 0)
			if measured {
				record["gasKnown"] = true
			} else {
				delete(record, "gas")
			}
			s, _ := aggregateFixture(t, []map[string]any{record}, nil)
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Totals struct {
					Known    bool   `json:"gasTotalKnown"`
					Exact    string `json:"gasTotalExact"`
					Samples  int    `json:"gasSamples"`
					Expected int    `json:"gasExpected"`
				} `json:"totals"`
			}
			if json.Unmarshal(raw, &wire) != nil || wire.Totals.Known != measured || wire.Totals.Expected != 1 || measured && (wire.Totals.Samples != 1 || wire.Totals.Exact != "0") {
				t.Fatal("gas availability did not distinguish missing data from measured zero")
			}
			if !measured && !strings.Contains(UsersMarkdown(s), "| Gas used | n/a |") {
				t.Fatal("missing gas became a measured total")
			}
		})
	}
}

func TestUserStorageSummaryRetainsExactOperands(t *testing.T) {
	p := &PhaseRecord{Before: &Snapshot{Dirs: map[string]DirSize{"synthetic:/data": {Bytes: 0}}, Index: &bench.StorageReport{PageSize: math.MaxInt64, Pages: 0}},
		After: &Snapshot{Dirs: map[string]DirSize{"synthetic:/data": {Bytes: math.MaxInt64}}, Index: &bench.StorageReport{PageSize: math.MaxInt64, Pages: math.MaxInt64}}}
	s, dir := aggregateFixture(t, []map[string]any{aggregateTx(1000, 1), aggregateTx(2000, 1)}, p)
	before, err := os.ReadFile(filepath.Join(dir, "00-users-synthetic.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Storage []struct {
			Name        string `json:"name"`
			Delta       string `json:"deltaExact"`
			Denominator int    `json:"confirmedRecords"`
			Known       bool   `json:"perConfirmedKnown"`
		} `json:"storage"`
	}
	if json.Unmarshal(raw, &wire) != nil {
		t.Fatal("summary JSON was unreadable")
	}
	found := false
	for _, row := range wire.Storage {
		if row.Name == "index (SQLite pages in use)" {
			found = row.Delta == "85070591730234615847396907784232501249" && row.Denominator == 2 && row.Known
		}
	}
	if !found {
		t.Fatal("storage report omitted exact wide operands or its real denominator")
	}
	after, err := os.ReadFile(filepath.Join(dir, "00-users-synthetic.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("summary rewrote retained phase evidence")
	}
}

func TestUserAggregatesKeepTransactionAndActionKindsSeparate(t *testing.T) {
	rows := []map[string]any{}
	for i, kind := range []string{"read", "order", "payout", "tx"} {
		rows = append(rows, map[string]any{"t": 1000 + i*1000, "profile": "light", "kind": kind, "action": "synthetic-shared-name", "ok": true})
	}
	s, _ := aggregateFixture(t, rows, nil)
	if len(s.Profiles) != 1 || s.Profiles[0].N != 1 || s.Profiles[0].OK != 1 {
		t.Error("orders or payouts inflated profile transaction counts")
	}
	if len(s.Actions) != 4 {
		t.Error("distinct action kinds were combined by name")
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Profiles []struct {
			Orders  int `json:"orders"`
			Payouts int `json:"payouts"`
		} `json:"profiles"`
	}
	if json.Unmarshal(raw, &wire) != nil || len(wire.Profiles) != 1 || wire.Profiles[0].Orders != 1 || wire.Profiles[0].Payouts != 1 {
		t.Error("profile aggregation lost separate nontransaction counts")
	}
}
