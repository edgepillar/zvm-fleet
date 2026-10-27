package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestClaimedStreamCleanAppendRetainsIdentityAndClosesOnce(t *testing.T) {
	e, f := streamBindingOpened(t)
	path := filepath.Join(e.Out, "records.jsonl")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := f.Write(nil); n != 0 || err != nil {
		t.Fatal("empty write changed a clean stream", err)
	}
	if n, err := f.Write([]byte("second prefix\n")); n != len("second prefix\n") || err != nil {
		t.Fatal("clean append failed", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal("clean close was not idempotent", err)
	}
	after, statErr := os.Stat(path)
	raw, readErr := os.ReadFile(path)
	if statErr != nil || readErr != nil || !os.SameFile(before, after) || string(raw) != "retained synthetic prefix\nsecond prefix\n" {
		t.Fatal("clean append lost file identity or accepted bytes")
	}
	if n, err := f.Write([]byte("after close\n")); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("closed stream accepted more output", err)
	}
}

func TestClaimedStreamConcurrentWritesRetainCompleteRows(t *testing.T) {
	e := &Env{}
	boundaryClaim(t, e)
	f, err := e.openUserOutput("records.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw := []byte(fmt.Sprintf("{\"synthetic\":%d}\n", i))
			if n, err := f.Write(raw); n != len(raw) || err != nil {
				t.Error("concurrent claimed write failed", err)
			}
		}()
	}
	wg.Wait()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(e.Out, "records.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, line := range bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n")) {
		var row struct {
			Synthetic int `json:"synthetic"`
		}
		if err := json.Unmarshal(line, &row); err != nil || seen[row.Synthetic] {
			t.Fatal("concurrent writes lost complete distinct rows", err)
		}
		seen[row.Synthetic] = true
	}
	if len(seen) != 64 {
		t.Fatal("concurrent claimed output omitted rows")
	}
}

func TestClaimedStreamRestoredNameDoesNotResumeFailedOutput(t *testing.T) {
	e, f := streamBindingOpened(t)
	streamBindingChange(t, e, "records.jsonl", "regular", f)
	_, first := f.Write([]byte("refused\n"))
	if first == nil {
		t.Fatal("fixture did not produce a refusal")
	}
	path := filepath.Join(e.Out, "records.jsonl")
	foreign := filepath.Join(e.Out, "foreign-retained")
	if err := os.Rename(path, foreign); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(e.Out, "retained-records.jsonl"), path); err != nil {
		t.Fatal(err)
	}
	views := streamBindingViews(t, []string{path, foreign})
	if n, again := f.Write([]byte("still refused\n")); n != 0 || !errors.Is(again, first) {
		t.Fatal("restored name resumed a failed stream", again)
	}
	if err := f.Close(); !errors.Is(err, first) {
		t.Fatal("close lost the original sticky refusal", err)
	}
	streamBindingPreserved(t, views)
}
