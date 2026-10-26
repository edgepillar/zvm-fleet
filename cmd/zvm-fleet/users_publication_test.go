package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func publicationFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, raw := range map[string]string{
		"records.jsonl": writerReportRecord,
		"samples.jsonl": "{\"t\":1000}\n",
		"USERS.md":      "retained synthetic Markdown\n",
		"summary.json":  "retained synthetic summary\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func publicationRetained(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != want {
		t.Error("publication changed retained bytes: " + filepath.Base(path))
	}
}

func TestUserReportRefusesReservedOutput(t *testing.T) {
	for _, name := range []string{"records.jsonl", "samples.jsonl", "users.json", "names.json", "user-run.json", "state.json", "00-users-synthetic.json", "summary.json"} {
		t.Run(name, func(t *testing.T) {
			dir := publicationFixture(t)
			if err := writeUsersReport(dir, filepath.Join(dir, name)); err == nil {
				t.Error("reserved report output was accepted")
			}
			publicationRetained(t, filepath.Join(dir, "records.jsonl"), writerReportRecord)
			publicationRetained(t, filepath.Join(dir, "samples.jsonl"), "{\"t\":1000}\n")
			publicationRetained(t, filepath.Join(dir, "USERS.md"), "retained synthetic Markdown\n")
			publicationRetained(t, filepath.Join(dir, "summary.json"), "retained synthetic summary\n")
			if strings.HasSuffix(name, ".json") && name != "summary.json" {
				if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Error("refusal created reserved context or phase output")
				}
			}
		})
	}
}

func TestUserReportSummaryRefusalPreservesMarkdown(t *testing.T) {
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := publicationFixture(t)
			path := filepath.Join(dir, "summary.json")
			if err := os.Rename(path, filepath.Join(dir, "retained-summary")); err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(t.TempDir(), "retained.json")
			if kind == "directory" {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(other, []byte("retained unrelated evidence\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, path); err != nil {
					t.Skipf("symlink fixture unavailable: %v", err)
				}
			}
			if err := writeUsersReport(dir, filepath.Join(dir, "USERS.md")); err == nil {
				t.Error("unusable summary destination was accepted")
			}
			publicationRetained(t, filepath.Join(dir, "USERS.md"), "retained synthetic Markdown\n")
			publicationRetained(t, filepath.Join(dir, "retained-summary"), "retained synthetic summary\n")
			if kind == "symlink" {
				publicationRetained(t, other, "retained unrelated evidence\n")
			}
		})
	}
}

func TestUserReportRefusesRawInputAliases(t *testing.T) {
	for _, kind := range []string{"markdown-symlink", "markdown-hardlink", "summary-symlink", "summary-hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := publicationFixture(t)
			out, alias := filepath.Join(dir, "USERS.md"), ""
			if strings.HasPrefix(kind, "markdown") {
				out = filepath.Join(t.TempDir(), "external.md")
				alias = out
			} else {
				alias = filepath.Join(dir, "summary.json")
				if err := os.Rename(alias, filepath.Join(dir, "retained-summary")); err != nil {
					t.Fatal(err)
				}
			}
			link := os.Link
			if strings.HasSuffix(kind, "symlink") {
				link = os.Symlink
			}
			if err := link(filepath.Join(dir, "records.jsonl"), alias); err != nil {
				t.Skipf("alias fixture unavailable: %v", err)
			}
			if err := writeUsersReport(dir, out); err == nil {
				t.Error("raw-input output alias was accepted")
			}
			publicationRetained(t, filepath.Join(dir, "records.jsonl"), writerReportRecord)
			publicationRetained(t, filepath.Join(dir, "samples.jsonl"), "{\"t\":1000}\n")
		})
	}
}

func TestUserReportRefusesSharedOutputIdentity(t *testing.T) {
	dir := publicationFixture(t)
	if err := os.Rename(filepath.Join(dir, "USERS.md"), filepath.Join(dir, "retained-markdown")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "summary.json"), filepath.Join(dir, "USERS.md")); err != nil {
		t.Skipf("hardlink fixture unavailable: %v", err)
	}
	if err := writeUsersReport(dir, filepath.Join(dir, "USERS.md")); err == nil {
		t.Error("shared Markdown/summary file identity was accepted")
	}
	publicationRetained(t, filepath.Join(dir, "summary.json"), "retained synthetic summary\n")
}

func TestUserReportRefusesExistingPublisherClaim(t *testing.T) {
	dir := publicationFixture(t)
	const retained = "retained synthetic publisher claim\n"
	if err := os.WriteFile(filepath.Join(dir, "users-report.lock"), []byte(retained), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeUsersReport(dir, filepath.Join(dir, "USERS.md")); err == nil {
		t.Error("existing publisher claim was ignored")
	}
	publicationRetained(t, filepath.Join(dir, "USERS.md"), "retained synthetic Markdown\n")
	publicationRetained(t, filepath.Join(dir, "summary.json"), "retained synthetic summary\n")
	publicationRetained(t, filepath.Join(dir, "users-report.lock"), retained)
}
