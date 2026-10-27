package fleet

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func storageObservationAmbiguous() []struct{ name, raw string } {
	base := `{"fileBytes":17,"pages":3,"freePages":1,"pageSize":4096`
	return []struct{ name, raw string }{
		{"file-case", base + `,"FileBytes":0}`},
		{"pages-case", base + `,"Pages":1}`},
		{"free-case", base + `,"FreePages":0}`},
		{"size-case", base + `,"PageSize":1}`},
		{"file-duplicate", base + `,"fileBytes":0}`},
		{"pages-duplicate", base + `,"pages":1}`},
		{"free-duplicate", base + `,"freePages":0}`},
		{"size-duplicate", base + `,"pageSize":1}`},
		{"escaped-duplicate", base + `,"fileBytes":0}`},
		{"nested-duplicate", base + `,"futureExtension":{"sample":1,"sample":2}}`},
	}
}

func TestStorageProbeRefusesAmbiguousMeasurements(t *testing.T) {
	for _, tc := range storageObservationAmbiguous() {
		t.Run(tc.name, func(t *testing.T) {
			calls := resourceDocker(t, tc.raw)
			e := &Env{running: map[string]bool{"synthetic-node": true}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			report, file, pages, err := e.probeObserved(ctx, "synthetic-node", false)
			raw, readErr := os.ReadFile(calls)
			if readErr != nil || !strings.Contains(string(raw), "exec synthetic-node /tmp/zvm-probe bench storage") {
				t.Fatal("synthetic probe command was not observed", readErr)
			}
			if err == nil || report != nil || file || pages {
				t.Fatal("ambiguous probe fields became a usable storage observation")
			}
		})
	}
}

func TestStorageSnapshotRefusesAmbiguousMeasurements(t *testing.T) {
	for _, name := range []string{"index", "followerDb"} {
		for _, tc := range storageObservationAmbiguous() {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				raw := []byte(`{"` + name + `":` + tc.raw + `,"indexFileBytesKnown":true}`)
				var value Snapshot
				if err := json.Unmarshal(raw, &value); err == nil {
					t.Fatal("ambiguous retained storage fields were accepted")
				}
			})
		}
	}
}

func TestStorageSnapshotRefusesAmbiguousEnvelope(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"duplicate-index", `{"index":{"fileBytes":1},"index":{"fileBytes":2}}`},
		{"duplicate-follower", `{"followerDb":{"pages":1},"followerDb":{"pages":2}}`},
		{"duplicate-declaration", `{"indexPagesKnown":true,"indexPagesKnown":false}`},
		{"index-case", `{"Index":{"fileBytes":0},"indexFileBytesKnown":true}`},
		{"follower-case", `{"FollowerDb":{"pages":0,"freePages":0,"pageSize":4096}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var value Snapshot
			if err := json.Unmarshal([]byte(tc.raw), &value); err == nil {
				t.Fatal("ambiguous snapshot envelope was accepted")
			}
		})
	}
}

func TestStorageObservationCanonicalShapesKeepAvailability(t *testing.T) {
	for _, tc := range []struct {
		name, raw   string
		file, pages bool
	}{
		{"empty", `{}`, false, false},
		{"zero", `{"fileBytes":0,"pages":0,"freePages":0,"pageSize":4096}`, true, true},
		{"file-only", `{"fileBytes":17}`, true, false},
		{"pages-only", `{"pages":3,"freePages":1,"pageSize":4096}`, false, true},
		{"null-file", `{"fileBytes":null,"pages":0,"freePages":0,"pageSize":4096}`, false, true},
		{"missing-free", `{"fileBytes":17,"pages":3,"pageSize":4096}`, true, false},
		{"all-null", `{"fileBytes":null,"pages":null,"freePages":null,"pageSize":null}`, false, false},
		{"extension", `{"fileBytes":17,"futureExtension":{"sample":1}}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resourceDocker(t, tc.raw)
			e := &Env{running: map[string]bool{"synthetic-node": true}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			report, file, pages, err := e.probeObserved(ctx, "synthetic-node", true)
			if err != nil || report == nil || file != tc.file || pages != tc.pages {
				t.Fatal("canonical capture changed independent storage availability", err)
			}
			var captured Snapshot
			raw := []byte(`{"index":` + tc.raw + `,"indexFileBytesKnown":true,"indexPagesKnown":true}`)
			if err := json.Unmarshal(raw, &captured); err != nil || (snapshotFileBytes(&captured) != nil) != tc.file || (snapshotIndex(&captured, false) != nil) != tc.pages {
				t.Fatal("canonical snapshot changed independent availability", err)
			}
			wire, err := json.Marshal(captured)
			if err != nil {
				t.Fatal(err)
			}
			var replay Snapshot
			if err := json.Unmarshal(wire, &replay); err != nil || (snapshotFileBytes(&replay) != nil) != tc.file || (snapshotIndex(&replay, false) != nil) != tc.pages {
				t.Fatal("canonical snapshot roundtrip changed availability", err)
			}
			var legacy Snapshot
			if err := json.Unmarshal([]byte(`{"index":`+tc.raw+`}`), &legacy); err != nil {
				t.Fatal(err)
			}
			wantLegacyFile := tc.file && report.FileBytes > 0
			if (snapshotFileBytes(&legacy) != nil) != wantLegacyFile || (snapshotIndex(&legacy, false) != nil) != tc.pages {
				t.Fatal("legacy unknown zero or canonical page availability changed")
			}
			var follower Snapshot
			if err := json.Unmarshal([]byte(`{"followerDb":`+tc.raw+`,"followerPagesKnown":true}`), &follower); err != nil || (snapshotIndex(&follower, true) != nil) != tc.pages {
				t.Fatal("follower availability changed", err)
			}
		})
	}
}

func TestStorageObservationAbsentContainerDoesNotProbe(t *testing.T) {
	calls := resourceDocker(t, `{}`)
	e := &Env{running: map[string]bool{}}
	report, file, pages, err := e.probeObserved(context.Background(), "synthetic-node", false)
	if err != nil || report != nil || file || pages {
		t.Fatal("absent container became an observation", err)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatal("absent container invoked the synthetic command")
	}
}

func TestStorageObservationInvalidProbeEnvelopeStaysRefused(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `42`, `{`, `{} {}`} {
		t.Run(raw, func(t *testing.T) {
			resourceDocker(t, raw)
			e := &Env{running: map[string]bool{"synthetic-node": true}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			report, file, pages, err := e.probeObserved(ctx, "synthetic-node", false)
			if err == nil || report != nil || file || pages {
				t.Fatal("invalid probe envelope became available")
			}
		})
	}
}
