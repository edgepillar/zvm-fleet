package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const writerReportRecord = `{"t":1000,"profile":"light","action":"synthetic-read","kind":"read","ok":true}` + "\n"

func TestFinishUsersRunClosesBeforeReport(t *testing.T) {
	dir := t.TempDir()
	closed := 0
	err := finishUsersRun(nil, func() error {
		closed++
		return os.WriteFile(filepath.Join(dir, "records.jsonl"), []byte(writerReportRecord), 0o600)
	}, dir, filepath.Join(dir, "USERS.md"))
	if err != nil || closed != 1 {
		t.Fatal("report replay ran before closing the stream")
	}
	for _, name := range []string{"USERS.md", "summary.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal("finalized run omitted its report")
		}
	}
}

func TestFinishUsersRunRetainsRunCloseAndReportFailures(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "records.jsonl"), []byte(writerReportRecord), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "report-directory")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	runErr, closeErr := errors.New("synthetic run failure"), errors.New("synthetic close failure")
	err := finishUsersRun(runErr, func() error { return closeErr }, dir, out)
	var pathErr *os.PathError
	if !errors.Is(err, runErr) || !errors.Is(err, closeErr) || !errors.As(err, &pathErr) || pathErr.Path != out {
		t.Fatal("final report handling replaced a run, close or report failure")
	}
}

func TestFinishUsersRunRetainsExplicitPartialReport(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "records.jsonl"), []byte(writerReportRecord+`{"t":2000`), 0o600); err != nil {
		t.Fatal(err)
	}
	want := errors.New("synthetic interrupted run")
	err := finishUsersRun(want, func() error { return nil }, dir, filepath.Join(dir, "USERS.md"))
	if !errors.Is(err, want) {
		t.Fatal("writing a partial report erased the run failure")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary struct {
		Inputs struct {
			Partial       bool   `json:"partial"`
			RunCompletion string `json:"runCompletion"`
		} `json:"inputs"`
	}
	if json.Unmarshal(raw, &summary) != nil || !summary.Inputs.Partial || summary.Inputs.RunCompletion != "not-established" {
		t.Fatal("partial report lost its evidence boundary")
	}
	md, err := os.ReadFile(filepath.Join(dir, "USERS.md"))
	if err != nil || !strings.Contains(string(md), "PARTIAL INPUT") {
		t.Fatal("partial Markdown omitted its visible marker")
	}
}
