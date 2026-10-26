package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUserJSONLCorruptionPreservesExistingOutputs(t *testing.T) {
	dir := t.TempDir()
	inputs := map[string]string{
		"records.jsonl": "{\"t\":1000,\"profile\":\"light\",\"action\":\"synthetic-read\",\"kind\":\"read\",\"ok\":true}\n{\n",
		"samples.jsonl": "{\"t\":1000}\n", "USERS.md": "retained synthetic report\n", "summary.json": "retained synthetic summary\n",
	}
	for name, raw := range inputs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeUsersReport(dir, filepath.Join(dir, "USERS.md")); err == nil {
		t.Error("CLI accepted a corrupt record stream")
	}
	for name, original := range inputs {
		if raw, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(raw) != original {
			t.Errorf("CLI corruption refusal did not preserve %s", name)
		}
	}
}
