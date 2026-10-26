package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUserContextCLIRefusalPreservesOutputs(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"records.jsonl":        writerReportRecord,
		"samples.jsonl":        "{\"t\":1000}\n",
		"00-users-first.json":  `{"id":"00-users-first"}`,
		"01-users-second.json": `{"id":"01-users-second"}`,
		"USERS.md":             "retained synthetic Markdown\n",
		"summary.json":         "retained synthetic summary\n",
	}
	for name, raw := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeUsersReport(dir, filepath.Join(dir, "USERS.md")); err == nil {
		t.Error("ambiguous user context did not refuse CLI output replacement")
	}
	for _, name := range []string{"USERS.md", "summary.json"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(raw) != files[name] {
			t.Error("context refusal did not preserve existing outputs")
		}
	}
}
