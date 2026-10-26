package fleet

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func accountingSnapshot() map[string]any {
	return map[string]any{
		"l2Head": uint64(10), "l2HeadKnown": true,
		"ops": map[string]any{"derivation": map[string]any{"totals": map[string]any{"txs": uint64(20), "dataBytes": uint64(30)}}},
		"dirs": map[string]any{
			"zvm-l1-pillar:/root/.znn/nom":             map[string]any{"bytes": 100, "allocated": 100},
			"zvm-l1-rpc:/root/.znn/nom":                map[string]any{"bytes": 100, "allocated": 100},
			"zvm-l1-rpc:/root/.znn/log":                map[string]any{"bytes": 100, "allocated": 100},
			"zvm-node:/data/media":                     map[string]any{"bytes": 100, "allocated": 100},
			"zvm-op-geth:/data/geth/chaindata":         map[string]any{"bytes": 100, "allocated": 100},
			"zvm-op-geth:/data/geth/chaindata/ancient": map[string]any{"bytes": 100, "allocated": 100},
		},
		"containers": map[string]any{
			"zvm-node":    map[string]any{"blkWrite": 100},
			"zvm-l1-rpc":  map[string]any{"blkWrite": 100},
			"zvm-op-geth": map[string]any{"blkWrite": 100},
		},
		"index":      map[string]any{"pages": 10, "freePages": 2, "pageSize": 1024},
		"followerDb": map[string]any{"pages": 10, "freePages": 2, "pageSize": 1024},
	}
}

func accountingTotal(s map[string]any, key string, value any) {
	s["ops"].(map[string]any)["derivation"].(map[string]any)["totals"].(map[string]any)[key] = value
}

func accountingDirectory(t *testing.T, before, after any) string {
	t.Helper()
	dir := t.TempDir()
	writeReportJSON(t, filepath.Join(dir, "00-accounting.json"), map[string]any{
		"id": "00-accounting", "before": before, "after": after,
	})
	return dir
}

func accountingRow(t *testing.T, before, after any) []string {
	t.Helper()
	md, n, err := Report(accountingDirectory(t, before, after))
	if err != nil || n != 1 {
		t.Fatalf("accounting report: phases=%d error=%v", n, err)
	}
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "| 00-accounting |") {
			parts := strings.Split(line, "|")[1:]
			parts = parts[:len(parts)-1]
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			if len(parts) != 15 {
				t.Fatalf("storage column count = %d", len(parts))
			}
			return parts
		}
	}
	t.Fatal("phase with unavailable measurements disappeared from the report")
	return nil
}

func TestAccountingAPIKeepsNumericTokens(t *testing.T) {
	for _, value := range []string{"9007199254740993", "18446744073709551615", "1.25", "1.001e3"} {
		t.Run(value, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"counter":` + value + `}`))
			}))
			defer srv.Close()
			e := &Env{API: srv.URL, http: srv.Client()}
			var out map[string]any
			if err := e.getJSON(context.Background(), "/measurement", &out); err != nil {
				t.Fatal(err)
			}
			got, ok := out["counter"].(json.Number)
			if !ok || string(got) != value {
				t.Fatal("API number token was coerced instead of retained")
			}
		})
	}
}

func TestAccountingAPIRejectsTrailingDocument(t *testing.T) {
	for _, body := range []string{`{"counter":1}{}`, `{"counter":1}garbage`} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer srv.Close()
			e := &Env{API: srv.URL, http: srv.Client()}
			var out map[string]any
			if err := e.getJSON(context.Background(), "/measurement", &out); err == nil {
				t.Fatal("partial JSON response was accepted")
			}
		})
	}
}

func TestAccountingExactDerivedDelta(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after any
		want          string
	}{
		{"above-safe-integer", uint64(9007199254740992), uint64(9007199254740993), "1"},
		{"adjacent-large-integers", uint64(9007199254740993), uint64(9007199254740994), "1"},
		{"full-width", uint64(0), uint64(math.MaxUint64), "18,446,744,073,709,551,615"},
		{"adjacent-maximum", uint64(math.MaxUint64 - 1), uint64(math.MaxUint64), "1"},
		{"decimal-strings", "0", "18446744073709551615", "18,446,744,073,709,551,615"},
		{"integral-exponent", json.Number("1.000e3"), json.Number("1.001e3"), "1"},
		{"measured-zero", uint64(0), uint64(0), "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := accountingSnapshot(), accountingSnapshot()
			accountingTotal(before, "txs", tc.before)
			accountingTotal(after, "txs", tc.after)
			if got := accountingRow(t, before, after)[2]; got != tc.want {
				t.Fatalf("derived count = %q, want exact %q", got, tc.want)
			}
		})
	}
}

func TestAccountingUnknownOrResetCounter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"missing", nil}, {"negative", -1}, {"fractional", 1.5},
		{"overflow", json.Number("18446744073709551616")}, {"bad-string", "unknown"},
		{"overflow-string", "18446744073709551616"}, {"fractional-string", "3.5"},
		{"boolean", true}, {"object", map[string]any{}}, {"reset", 19},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := accountingSnapshot(), accountingSnapshot()
			accountingTotal(after, "txs", tc.value)
			if got := accountingRow(t, before, after)[2]; got != "n/a" {
				t.Fatalf("unavailable or reset counter became %q", got)
			}
		})
	}
}

func TestAccountingUnavailableStorage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		column int
		change func(map[string]any)
	}{
		{"ops", 2, func(s map[string]any) { delete(s, "ops") }},
		{"derivation", 2, func(s map[string]any) { s["ops"].(map[string]any)["derivation"] = nil }},
		{"totals", 2, func(s map[string]any) { s["ops"].(map[string]any)["derivation"].(map[string]any)["totals"] = nil }},
		{"directory", 4, func(s map[string]any) { delete(s["dirs"].(map[string]any), "zvm-l1-pillar:/root/.znn/nom") }},
		{"negative-directory", 4, func(s map[string]any) {
			s["dirs"].(map[string]any)["zvm-l1-pillar:/root/.znn/nom"] = map[string]any{"bytes": -1}
		}},
		{"engine-directory", 7, func(s map[string]any) { delete(s["dirs"].(map[string]any), "zvm-op-geth:/data/geth/chaindata") }},
		{"index", 9, func(s map[string]any) { delete(s, "index") }},
		{"invalid-index", 9, func(s map[string]any) { s["index"].(map[string]any)["freePages"] = 11 }},
		{"page-size", 9, func(s map[string]any) { s["index"].(map[string]any)["pageSize"] = 0 }},
		{"follower", 10, func(s map[string]any) { delete(s, "followerDb") }},
		{"container", 13, func(s map[string]any) { delete(s["containers"].(map[string]any), "zvm-node") }},
		{"write-reset", 13, func(s map[string]any) { s["containers"].(map[string]any)["zvm-node"] = map[string]any{"blkWrite": 0} }},
		{"engine-write-reset", 12, func(s map[string]any) {
			s["containers"].(map[string]any)["zvm-op-geth"] = map[string]any{"blkWrite": 0}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := accountingSnapshot(), accountingSnapshot()
			tc.change(after)
			if got := accountingRow(t, before, after)[tc.column]; got != "n/a" {
				t.Fatalf("unavailable storage became %q", got)
			}
		})
	}
}

func TestAccountingMissingSnapshotRemainsVisible(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after any
	}{
		{"before", nil, accountingSnapshot()}, {"after", accountingSnapshot(), nil}, {"both", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, cell := range accountingRow(t, tc.before, tc.after)[1:] {
				if cell != "n/a" {
					t.Fatalf("missing snapshot column %d = %q", i+1, cell)
				}
			}
		})
	}
}

func TestAccountingHeadAvailability(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after uint64
		known         any
		want          string
	}{
		{"unavailable", 10, 11, false, "n/a"}, {"legacy-zero", 0, 0, nil, "n/a"},
		{"known-zero", 0, 0, true, "0"}, {"reset", 10, 9, true, "n/a"},
		{"full-width", 0, math.MaxUint64, true, "18446744073709551615"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := accountingSnapshot(), accountingSnapshot()
			before["l2Head"], after["l2Head"] = tc.before, tc.after
			before["l2HeadKnown"], after["l2HeadKnown"] = tc.known, tc.known
			if got := accountingRow(t, before, after)[1]; got != tc.want {
				t.Fatalf("head delta = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAccountingSQLiteProductAndPruning(t *testing.T) {
	for _, tc := range []struct {
		name              string
		pages, free, size int64
		want              string
	}{
		{"pruning", 8, 2, 1024, "-2.0 KB"},
		{"full-product", math.MaxInt64, 0, 65536, "562949953421312.00 GB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := accountingSnapshot(), accountingSnapshot()
			if tc.name == "full-product" {
				before["index"] = map[string]any{"pages": 0, "freePages": 0, "pageSize": 65536}
			}
			after["index"] = map[string]any{"pages": tc.pages, "freePages": tc.free, "pageSize": tc.size}
			if got := accountingRow(t, before, after)[9]; got != tc.want {
				t.Fatalf("SQLite delta = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAccountingFullWidthFormatting(t *testing.T) {
	t.Run("signed-count", func(t *testing.T) {
		if got := commas(math.MinInt64); got != "-9,223,372,036,854,775,808" {
			t.Fatalf("signed count = %q", got)
		}
	})
	t.Run("signed-bytes", func(t *testing.T) {
		if got := signedBytes(math.MinInt64); got != "-8589934592.00 GB" {
			t.Fatalf("signed bytes = %q", got)
		}
	})
}

func TestAccountingUnsignedGasMean(t *testing.T) {
	dir, _ := reportFixture(t)
	res := reportResult()
	res.Records = res.Records[:1]
	res.Records[0].GasUsed = math.MaxUint64
	writeReportJSON(t, filepath.Join(dir, "runs", "result.json"), res)
	md, n, err := Report(dir)
	if err != nil || n != 1 {
		t.Fatalf("gas report: phases=%d error=%v", n, err)
	}
	if !strings.Contains(md, "18,446,744,073,709,551,615") {
		t.Fatal("unsigned gas mean wrapped into a signed count")
	}
}

func TestAccountingBadNamesNeverSelectFallback(t *testing.T) {
	for _, body := range []string{"null", "{}", "[]", "garbage", `{"Node":"custom"}{}`} {
		t.Run(body, func(t *testing.T) {
			dir := accountingDirectory(t, accountingSnapshot(), accountingSnapshot())
			if err := os.WriteFile(filepath.Join(dir, "names.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			md, n, err := Report(dir)
			if err == nil || md != "" || n != 0 {
				t.Fatal("invalid names selected a fallback profile")
			}
		})
	}
}
