package fleet

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

func reportResult() *bench.Result {
	return &bench.Result{
		Scenario: "transfer-new", Lane: "batch", Wallets: 2,
		StartedAt: time.Unix(1, 0).UTC(), EndedAt: time.Unix(6, 0).UTC(),
		Records: []bench.Record{
			{Wallet: 0, SubmitMS: 1000, IncludedMS: 2000, Block: 1, Receipt: true, Status: 1, GasUsed: 21000},
			{Wallet: 0, SubmitMS: 2000, IncludedMS: 3000, Block: 2, Receipt: true, Status: 0, GasUsed: 20000},
			// An inclusion without a fetched receipt does not prove a revert.
			{Wallet: 1, SubmitMS: 3000, IncludedMS: 4000, Block: 3},
			{Wallet: 1, SubmitMS: 4000, Error: "synthetic refused"},
		},
		Blocks:  []bench.BlockStat{{Number: 1, Timestamp: 2}, {Number: 2, Timestamp: 3}, {Number: 3, Timestamp: 4}},
		Summary: bench.Summary{Submitted: 777, Included: 777, Reverted: 777},
	}
}

func writeReportJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func reportFixture(t *testing.T) (string, *PhaseRecord) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "results")
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := &PhaseRecord{ID: "00-test", Runs: []RunRef{{
		File: "runs/result.json", Scenario: "transfer-new", Lane: "batch", Wallets: 2, Rate: 5,
		Senders: 777, Summary: bench.Summary{Submitted: 777, Included: 777, Reverted: 777},
		Errors: map[string]int{"stored-error": 777},
	}}}
	writeReportJSON(t, filepath.Join(dir, "00-test.json"), p)
	return dir, p
}

func TestReportRequiresReadableMatchingRun(t *testing.T) {
	for _, kind := range []string{"missing", "directory", "malformed", "null", "empty-object", "scenario", "lane", "wallets"} {
		t.Run(kind, func(t *testing.T) {
			dir, _ := reportFixture(t)
			path := filepath.Join(dir, "runs", "result.json")
			res := reportResult()
			switch kind {
			case "missing":
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "malformed", "null", "empty-object":
				raw := map[string]string{"malformed": `{"records":[`, "null": "null", "empty-object": "{}"}[kind]
				if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
			default:
				switch kind {
				case "scenario":
					res.Scenario = "different-workload"
				case "lane":
					res.Lane = "native"
				case "wallets":
					res.Wallets++
				}
				writeReportJSON(t, path, res)
			}
			md, n, err := Report(dir)
			if err == nil || md != "" || n != 0 {
				t.Fatalf("unusable raw run produced a report: phases=%d error=%v", n, err)
			}
		})
	}
}

func TestReportRunReferenceStaysInResultsDirectory(t *testing.T) {
	for _, kind := range []string{"empty", "parent", "absolute"} {
		t.Run(kind, func(t *testing.T) {
			dir, p := reportFixture(t)
			outside := filepath.Join(filepath.Dir(dir), "outside.json")
			writeReportJSON(t, outside, reportResult())
			switch kind {
			case "empty":
				p.Runs[0].File = ""
			case "parent":
				p.Runs[0].File = "../outside.json"
			case "absolute":
				p.Runs[0].File = filepath.ToSlash(outside)
			}
			writeReportJSON(t, filepath.Join(dir, "00-test.json"), p)
			md, n, err := Report(dir)
			if err == nil || md != "" || n != 0 {
				t.Fatalf("nonlocal reference produced a report: phases=%d error=%v", n, err)
			}
		})
	}
}

func TestReportRebuildsAllRunCounters(t *testing.T) {
	dir, _ := reportFixture(t)
	writeReportJSON(t, filepath.Join(dir, "runs", "result.json"), reportResult())
	md, n, err := Report(dir)
	if err != nil || n != 1 {
		t.Fatalf("report: phases=%d error=%v", n, err)
	}
	if !strings.Contains(md, "| 00-test | transfer-new | batch | 2 | 2 | 5 | 3 | 1 | 1 |") {
		t.Fatal("run counters do not reflect the raw records")
	}
	for _, want := range []string{"1 \u00d7 reverted", "1 \u00d7 synthetic refused"} {
		if !strings.Contains(md, want) {
			t.Fatalf("missing recomputed error count %q", want)
		}
	}
	for _, stale := range []string{"777", "stored-error", "funding only"} {
		if strings.Contains(md, stale) {
			t.Fatalf("retained stale derived data %q", stale)
		}
	}
}

func TestReportClearsSummaryForZeroRecordRun(t *testing.T) {
	for _, records := range [][]bench.Record{nil, {}} {
		label := "null-records"
		if records != nil {
			label = "empty-records"
		}
		t.Run(label, func(t *testing.T) {
			dir, _ := reportFixture(t)
			res := reportResult()
			res.Records, res.Blocks = records, nil
			writeReportJSON(t, filepath.Join(dir, "runs", "result.json"), res)
			md, n, err := Report(dir)
			if err != nil || n != 1 {
				t.Fatalf("zero-record run: phases=%d error=%v", n, err)
			}
			if !strings.Contains(md, "funding only") || strings.Contains(md, "777") || strings.Contains(md, "stored-error") {
				t.Fatal("zero-record run retained cached summary or errors")
			}
		})
	}
}

func TestReportLateMissingRunEmitsNoPartialReport(t *testing.T) {
	dir, p := reportFixture(t)
	writeReportJSON(t, filepath.Join(dir, "runs", "result.json"), reportResult())
	p.ID, p.Runs[0].File = "01-later", "runs/missing.json"
	writeReportJSON(t, filepath.Join(dir, "01-later.json"), p)
	md, n, err := Report(dir)
	if err == nil || md != "" || n != 0 {
		t.Fatalf("late missing run returned partial success: phases=%d error=%v", n, err)
	}
}

func TestReportKeepsLegacyReceiptSemantics(t *testing.T) {
	dir, _ := reportFixture(t)
	res := reportResult()
	res.Records[1].Receipt = false // Older records use nonzero gas as receipt evidence.
	writeReportJSON(t, filepath.Join(dir, "runs", "result.json"), res)
	md, n, err := Report(dir)
	if err != nil || n != 1 {
		t.Fatalf("legacy report: phases=%d error=%v", n, err)
	}
	if !strings.Contains(md, "| 00-test | transfer-new | batch | 2 | 2 | 5 | 3 | 1 | 1 |") ||
		!strings.Contains(md, "1 \u00d7 reverted") {
		t.Fatal("legacy receipt evidence differs between summary and error counts")
	}
}

func TestReportErrorRowsSorted(t *testing.T) {
	dir, _ := reportFixture(t)
	res := reportResult()
	res.Records, res.Blocks = nil, nil
	for i := 15; i >= 0; i-- {
		res.Records = append(res.Records, bench.Record{Error: fmt.Sprintf("synthetic-error-%02d", i)})
	}
	writeReportJSON(t, filepath.Join(dir, "runs", "result.json"), res)
	md, n, err := Report(dir)
	if err != nil || n != 1 {
		t.Fatalf("error report: phases=%d error=%v", n, err)
	}
	parts := strings.SplitN(md, "## Errors\n\n", 2)
	if len(parts) != 2 {
		t.Fatal("missing errors section")
	}
	var want strings.Builder
	for i := 0; i < 16; i++ {
		fmt.Fprintf(&want, "- 00-test transfer-new/batch: 1 \u00d7 synthetic-error-%02d\n", i)
	}
	if parts[1] != want.String() {
		t.Fatal("error rows are not sorted by their recomputed keys")
	}
}
