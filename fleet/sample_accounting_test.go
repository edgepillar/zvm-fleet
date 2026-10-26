package fleet

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var sampleAccountingStart = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func writeSampleAccountingLines(t *testing.T, path string, rows []map[string]any) {
	t.Helper()
	var raw []byte
	for _, row := range rows {
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, b...)
		raw = append(raw, '\n')
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func sampleAccountingSummary(t *testing.T, rows []map[string]any) *UsersSummary {
	t.Helper()
	dir := t.TempDir()
	writeSampleAccountingLines(t, filepath.Join(dir, "records.jsonl"), []map[string]any{
		{"t": sampleAccountingStart.UnixMilli(), "profile": "light", "kind": "read", "action": "synthetic-read", "ok": true},
		{"t": sampleAccountingStart.Add(40 * time.Minute).UnixMilli(), "profile": "light", "kind": "read", "action": "synthetic-read", "ok": true},
	})
	if rows != nil {
		writeSampleAccountingLines(t, filepath.Join(dir, "samples.jsonl"), rows)
	}
	summary, err := SummarizeUsers(dir)
	if err != nil {
		t.Fatal(err)
	}
	return summary
}

func sampleAccountingObservation(seconds int, value uint64, known *bool) map[string]any {
	row := map[string]any{"t": sampleAccountingStart.Add(time.Duration(seconds) * time.Second).UnixMilli(), "l2": value, "l1": value}
	if known != nil {
		row["known"] = map[string]bool{"l2": *known, "l1": *known}
	}
	return row
}

func sampleAccountingCells(t *testing.T, summary *UsersSummary, window string) []string {
	t.Helper()
	for _, row := range strings.Split(UsersMarkdown(summary), "\n") {
		if strings.HasPrefix(row, "| "+window+" |") {
			cells := strings.Split(row, "|")
			cells = cells[1 : len(cells)-1]
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}
			if len(cells) != 15 {
				t.Fatalf("window column count = %d", len(cells))
			}
			return cells
		}
	}
	t.Fatalf("window %s disappeared", window)
	return nil
}

func TestSampleAccountingCounterWindows(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name string
		rows []map[string]any
		want string
	}{
		{"increasing", []map[string]any{sampleAccountingObservation(1, 10, &yes), sampleAccountingObservation(2, 13, &yes)}, "3"},
		{"stationary", []map[string]any{sampleAccountingObservation(1, 10, &yes), sampleAccountingObservation(2, 10, &yes)}, "0"},
		{"known-genesis", []map[string]any{sampleAccountingObservation(1, 0, &yes), sampleAccountingObservation(2, 3, &yes)}, "3"},
		{"reset", []map[string]any{sampleAccountingObservation(1, 10, &yes), sampleAccountingObservation(2, 4, &yes)}, "n/a"},
		{"intermediate-reset", []map[string]any{sampleAccountingObservation(1, 10, &yes), sampleAccountingObservation(2, 4, &yes), sampleAccountingObservation(3, 12, &yes)}, "n/a"},
		{"single-observation", []map[string]any{sampleAccountingObservation(1, 10, &yes)}, "n/a"},
		{"missing-samples", nil, "n/a"},
		{"unavailable-positive", []map[string]any{sampleAccountingObservation(1, 10, &no), sampleAccountingObservation(2, 13, &no)}, "n/a"},
		{"availability-gap", []map[string]any{sampleAccountingObservation(1, 10, &yes), sampleAccountingObservation(2, 0, &no), sampleAccountingObservation(3, 13, &yes)}, "n/a"},
		{"out-of-order", []map[string]any{sampleAccountingObservation(2, 10, &yes), sampleAccountingObservation(1, 13, &yes)}, "n/a"},
		{"duplicate-time", []map[string]any{sampleAccountingObservation(1, 10, &yes), sampleAccountingObservation(1, 10, &yes)}, "n/a"},
		{"conflicting-time", []map[string]any{sampleAccountingObservation(1, 10, &yes), sampleAccountingObservation(1, 13, &yes)}, "n/a"},
		{"legacy-positive", []map[string]any{sampleAccountingObservation(1, 10, nil), sampleAccountingObservation(2, 13, nil)}, "3"},
		{"legacy-zero", []map[string]any{sampleAccountingObservation(1, 0, nil), sampleAccountingObservation(2, 3, nil)}, "n/a"},
		{"full-width", []map[string]any{sampleAccountingObservation(1, math.MaxUint64-1, &yes), sampleAccountingObservation(2, math.MaxUint64, &yes)}, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cells := sampleAccountingCells(t, sampleAccountingSummary(t, tc.rows), "00:00")
			for _, column := range []int{8, 9} {
				if cells[column] != tc.want {
					t.Errorf("sample counter column %d = %q, want %q", column, cells[column], tc.want)
				}
			}
		})
	}
}

func TestSampleAccountingWindowIsolation(t *testing.T) {
	yes := true
	summary := sampleAccountingSummary(t, []map[string]any{
		sampleAccountingObservation(1, 10, &yes), sampleAccountingObservation(2, 4, &yes),
		sampleAccountingObservation(1801, 8, &yes), sampleAccountingObservation(1802, 11, &yes),
	})
	first, second := sampleAccountingCells(t, summary, "00:00"), sampleAccountingCells(t, summary, "00:30")
	if first[8] != "n/a" || first[9] != "n/a" || second[8] != "3" || second[9] != "3" {
		t.Fatal("a counter reset either wrapped or contaminated the next observation window")
	}
}

func TestSampleAccountingExplicitAvailabilityInSummary(t *testing.T) {
	yes := true
	summary := sampleAccountingSummary(t, []map[string]any{
		sampleAccountingObservation(1, 0, &yes), sampleAccountingObservation(2, 0, &yes),
	})
	for i, window := range summary.Windows {
		raw, err := json.Marshal(window)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"l2BlocksKnown", "l1MomentumsKnown"} {
			known, present := fields[key].(bool)
			if !present || known != (i == 0) {
				t.Errorf("window %d has no accurate %s availability", i, key)
			}
		}
	}
}
