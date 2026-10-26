package fleet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const userJSONLRecordA = `{"t":1000,"profile":"light","action":"synthetic-read","kind":"read","ok":true}`
const userJSONLRecordB = `{"t":2000,"profile":"light","action":"synthetic-read","kind":"read","ok":true}`
const userJSONLSampleA = `{"t":1000,"l1":10,"l2":10}`
const userJSONLSampleB = `{"t":2000,"l1":12,"l2":12}`

func userJSONLFixture(t *testing.T, records, samples string) string {
	t.Helper()
	dir := t.TempDir()
	for name, raw := range map[string]string{"records.jsonl": records, "samples.jsonl": samples} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestUserJSONLCompletedCorruptionStopsSummary(t *testing.T) {
	bad := []string{"{", "synthetic-sensitive-content", "null", "{}", "[]", "", "  ", userJSONLRecordA + "{}",
		`{"profile":"light","action":"synthetic-read","kind":"read"}`,
		`{"t":-1,"profile":"light","action":"synthetic-read","kind":"read"}`,
		`{"t":1000,"profile":"light","action":"synthetic-read"}`,
		`{"t":1000,"profile":"light","action":"synthetic-read","kind":"unknown"}`,
		`{"t":1000,"action":"synthetic-read","kind":"read"}`,
		`{"t":1000,"profile":"unknown","action":"synthetic-read","kind":"read"}`,
		`{"t":1000,"profile":"light","kind":"read"}`,
		`{"t":1000,"user":-1,"profile":"light","action":"synthetic-read","kind":"read"}`,
		`{"t":1000,"profile":"light","action":"synthetic-read","kind":"read","latencyMs":-1}`,
		`{"t":1000,"profile":"light","action":"synthetic-read","kind":"tx","ok":true,"reverted":true}`,
		`{"t":1000,"t":1500,"profile":"light","action":"synthetic-read","kind":"read"}`,
		`{"t":1000,"profile":"light","action":"synthetic-read","kind":"read","extra":{"key":1,"key":2}}`,
		"{\"t\":1000,\"profile\":\"light\",\"action\":\"bad\xff\",\"kind\":\"read\"}",
	}
	for i, row := range bad {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			records := userJSONLRecordA + "\n" + row + "\n" + userJSONLRecordB + "\n"
			dir := userJSONLFixture(t, records, userJSONLSampleA+"\n")
			summary, err := SummarizeUsers(dir)
			if err == nil || summary != nil {
				t.Error("completed corrupt record silently produced a summary")
			}
			if err != nil && strings.Contains(err.Error(), "synthetic-sensitive-content") {
				t.Error("raw corrupt input was echoed in the error")
			}
			if raw, err := os.ReadFile(filepath.Join(dir, "records.jsonl")); err != nil || string(raw) != records {
				t.Error("summary changed the retained corrupt input")
			}
		})
	}
}

func TestUserJSONLSampleCorruptionAndReadFailuresStopSummary(t *testing.T) {
	for _, raw := range []string{"{\n", "null\n", "{}\n", "\n", userJSONLSampleA + "{}\n", `{"t":-1}` + "\n",
		`{"t":1000,"t":1500}` + "\n", `{"t":1000,"l1":"invalid"}` + "\n"} {
		t.Run(raw, func(t *testing.T) {
			dir := userJSONLFixture(t, userJSONLRecordA+"\n"+userJSONLRecordB+"\n", userJSONLSampleA+"\n"+raw)
			if summary, err := SummarizeUsers(dir); err == nil || summary != nil {
				t.Fatal("corrupt sample stream silently produced resource coverage")
			}
		})
	}
	t.Run("directory", func(t *testing.T) {
		dir := userJSONLFixture(t, userJSONLRecordA+"\n", "")
		path := filepath.Join(dir, "samples.jsonl")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if summary, err := SummarizeUsers(dir); err == nil || summary != nil {
			t.Fatal("sample read failure was ignored")
		}
	})
	t.Run("overlong-row", func(t *testing.T) {
		dir := userJSONLFixture(t, userJSONLRecordA+"\n", userJSONLSampleA+"\n"+`{"t":2000,"padding":"`+strings.Repeat("x", 17<<20)+"\"}\n")
		if summary, err := SummarizeUsers(dir); err == nil || summary != nil {
			t.Fatal("sample scanner limit failure was ignored")
		}
	})
}

func TestUserJSONLInterruptedPrefixesRetainExplicitEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, records, samples, selected, state string
		rows                                    int64
	}{
		{"record-tail", userJSONLRecordA + "\n" + userJSONLRecordB + "\n" + `{"t":3000`, userJSONLSampleA + "\n", "records", "truncated-tail", 2},
		{"sample-tail", userJSONLRecordA + "\n" + userJSONLRecordB + "\n", userJSONLSampleA + "\n" + `{"t":3000`, "samples", "truncated-tail", 1},
		{"valid-no-newline", userJSONLRecordA + "\n" + userJSONLRecordB, userJSONLSampleA + "\n", "records", "unterminated", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := userJSONLFixture(t, tc.records, tc.samples)
			summary, err := SummarizeUsers(dir)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(summary)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Inputs struct {
					Partial bool            `json:"partial"`
					Records json.RawMessage `json:"records"`
					Samples json.RawMessage `json:"samples"`
				} `json:"inputs"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			selected, original := wire.Inputs.Records, tc.records
			if tc.selected == "samples" {
				selected, original = wire.Inputs.Samples, tc.samples
			}
			var evidence struct {
				State  string `json:"state"`
				Rows   int64  `json:"rows"`
				Bytes  int64  `json:"bytes"`
				SHA256 string `json:"sha256"`
			}
			if len(selected) == 0 || json.Unmarshal(selected, &evidence) != nil {
				t.Fatal("interrupted input prefix omitted its evidence")
			}
			digest := sha256.Sum256([]byte(original))
			if !wire.Inputs.Partial || evidence.State != tc.state || evidence.Rows != tc.rows || evidence.Bytes != int64(len(original)) || evidence.SHA256 != hex.EncodeToString(digest[:]) {
				t.Error("interrupted prefix lost its exact framing, rows, byte count or raw-input hash")
			}
			if !strings.Contains(UsersMarkdown(summary), "PARTIAL INPUT") {
				t.Error("partial retained input was presented without a visible partial marker")
			}
			for name, original := range map[string]string{"records.jsonl": tc.records, "samples.jsonl": tc.samples} {
				if raw, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(raw) != original {
					t.Error("interrupted input was repaired or rewritten")
				}
			}
		})
	}
}

func TestUserJSONLInvalidPresentMetadataStopsSummary(t *testing.T) {
	for _, name := range []string{"users.json", "00-users-synthetic.json"} {
		for _, raw := range []string{"null", "{", "{}{}"} {
			t.Run(name+raw, func(t *testing.T) {
				dir := userJSONLFixture(t, userJSONLRecordA+"\n", userJSONLSampleA+"\n")
				if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
				if summary, err := SummarizeUsers(dir); err == nil || summary != nil {
					t.Fatal("invalid present user metadata silently changed report context")
				}
			})
		}
	}
}
