package fleet

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const contextRunA = "00000000000000000000000000000001"
const contextRunB = "00000000000000000000000000000002"

func contextWriteJSON(t *testing.T, dir, name string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func contextMetadata() map[string]any {
	return map[string]any{"label": "synthetic", "mix": []any{
		map[string]any{"profile": map[string]any{"name": "light"}, "count": 1},
	}}
}

func contextPhase(meta map[string]any) map[string]any {
	return map[string]any{"id": "00-users-synthetic", "data": map[string]any{"users": meta}}
}

func contextLegacyFixture(t *testing.T) string {
	t.Helper()
	dir := userJSONLFixture(t, userJSONLRecordA+"\n"+userJSONLRecordB+"\n", userJSONLSampleA+"\n")
	meta := contextMetadata()
	contextWriteJSON(t, dir, "users.json", meta)
	contextWriteJSON(t, dir, "00-users-synthetic.json", contextPhase(meta))
	return dir
}

func TestUserContextRejectsAmbiguousPhases(t *testing.T) {
	dir := contextLegacyFixture(t)
	phase := contextPhase(contextMetadata())
	phase["id"] = "01-users-other"
	contextWriteJSON(t, dir, "01-users-other.json", phase)
	if s, err := SummarizeUsers(dir); err == nil || s != nil {
		t.Fatal("multiple user phases were silently selected by filename")
	}
}

func TestUserContextRejectsContradictoryMetadata(t *testing.T) {
	for _, name := range []string{"label", "population", "phase-id", "duplicate-profile", "outside-roster"} {
		t.Run(name, func(t *testing.T) {
			dir := contextLegacyFixture(t)
			meta, phase := contextMetadata(), contextPhase(contextMetadata())
			switch name {
			case "label":
				meta["label"] = "other"
			case "population":
				meta["mix"] = []any{map[string]any{"profile": map[string]any{"name": "light"}, "count": 2}}
			case "phase-id":
				phase["id"] = "01-users-other"
			case "duplicate-profile":
				meta["mix"] = []any{map[string]any{"profile": map[string]any{"name": "light"}, "count": 1}, map[string]any{"profile": map[string]any{"name": "light"}, "count": 1}}
				phase = contextPhase(meta)
			case "outside-roster":
				if err := os.WriteFile(filepath.Join(dir, "records.jsonl"), []byte(`{"t":1000,"user":1,"profile":"light","kind":"read","action":"synthetic-read","ok":true}`+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			contextWriteJSON(t, dir, "users.json", meta)
			contextWriteJSON(t, dir, "00-users-synthetic.json", phase)
			if s, err := SummarizeUsers(dir); err == nil || s != nil {
				t.Fatal("contradictory user metadata silently changed report context")
			}
		})
	}
}

func TestUserContextRejectsInvalidTemporalBounds(t *testing.T) {
	for _, tc := range []struct{ name, start, end string }{
		{"reversed", "1970-01-01T00:00:02Z", "1970-01-01T00:00:01Z"},
		{"end-without-start", "", "1970-01-01T00:00:02Z"},
		{"record-before-start", "1970-01-01T00:00:01.5Z", "1970-01-01T00:00:03Z"},
		{"record-after-end", "1970-01-01T00:00:01Z", "1970-01-01T00:00:01.5Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := contextLegacyFixture(t)
			phase := contextPhase(contextMetadata())
			data := phase["data"].(map[string]any)
			if tc.start != "" {
				data["simStart"] = tc.start
			}
			data["simEnd"] = tc.end
			contextWriteJSON(t, dir, "00-users-synthetic.json", phase)
			if s, err := SummarizeUsers(dir); err == nil || s != nil {
				t.Fatal("contradictory time bounds silently produced a report")
			}
		})
	}
}

func TestUserContextUsesDeclaredDenominator(t *testing.T) {
	dir := contextLegacyFixture(t)
	const row = `{"profile":"light","kind":"tx","action":"synthetic-tx","ok":true,`
	if err := os.WriteFile(filepath.Join(dir, "records.jsonl"), []byte(row+`"t":1000}`+"\n"+row+`"t":2000}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	phase := contextPhase(contextMetadata())
	data := phase["data"].(map[string]any)
	data["simStart"], data["simEnd"] = "1970-01-01T00:00:01Z", "1970-01-01T00:00:11Z"
	contextWriteJSON(t, dir, "00-users-synthetic.json", phase)
	s, err := SummarizeUsers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Start.Equal(time.UnixMilli(1000)) || !s.End.Equal(time.UnixMilli(11000)) || math.Abs(s.Totals.Hours-10.0/3600) > 1e-12 || math.Abs(s.Totals.TxPerSec-0.2) > 1e-12 || len(s.Windows) != 1 || math.Abs(s.Windows[0].TxPerSec-0.2) > 1e-12 {
		t.Fatal("minute padding or the last record replaced the declared rate denominator")
	}
}

func TestUserContextMarksLegacyBindingUnknown(t *testing.T) {
	dir := contextLegacyFixture(t)
	s, err := SummarizeUsers(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Inputs struct {
			Cohort struct {
				State string `json:"state"`
			} `json:"cohort"`
		} `json:"inputs"`
	}
	if json.Unmarshal(raw, &wire) != nil || wire.Inputs.Cohort.State != "legacy-unbound" {
		t.Fatal("legacy files omitted their unestablished common-run binding")
	}
}

func TestUserContextRejectsDeclaredTokenMismatches(t *testing.T) {
	for _, name := range []string{"records", "samples", "metadata", "phase", "profile", "marker", "record-missing", "sample-missing", "invalid-token"} {
		t.Run(name, func(t *testing.T) {
			dir := contextLegacyFixture(t)
			meta := contextMetadata()
			meta["runId"] = contextRunA
			phase := contextPhase(meta)
			phase["runId"] = contextRunA
			marker := map[string]any{"schema": 1, "kind": "users", "runId": contextRunA}
			namesRaw, _ := json.Marshal(V1Names())
			var names map[string]any
			if err := json.Unmarshal(namesRaw, &names); err != nil {
				t.Fatal(err)
			}
			names["runId"] = contextRunA
			recordID, sampleID := contextRunA, contextRunA
			switch name {
			case "records":
				recordID = contextRunB
			case "samples":
				sampleID = contextRunB
			case "metadata":
				meta["runId"] = contextRunB
			case "phase":
				phase["runId"] = contextRunB
			case "profile":
				names["runId"] = contextRunB
			case "marker":
				marker["runId"] = contextRunB
			case "record-missing":
				recordID = ""
			case "sample-missing":
				sampleID = ""
			case "invalid-token":
				recordID = "invalid"
			}
			record := map[string]any{"t": 1000, "profile": "light", "kind": "read", "action": "synthetic-read", "ok": true}
			sample := map[string]any{"t": 1000}
			if recordID != "" {
				record["runId"] = recordID
			}
			if sampleID != "" {
				sample["runId"] = sampleID
			}
			writeSampleAccountingLines(t, filepath.Join(dir, "records.jsonl"), []map[string]any{record})
			writeSampleAccountingLines(t, filepath.Join(dir, "samples.jsonl"), []map[string]any{sample})
			for file, value := range map[string]any{"users.json": meta, "00-users-synthetic.json": phase, "names.json": names, "user-run.json": marker} {
				contextWriteJSON(t, dir, file, value)
			}
			if s, err := SummarizeUsers(dir); err == nil || s != nil {
				t.Fatal("inconsistent declared run tokens were silently combined")
			}
		})
	}
}
