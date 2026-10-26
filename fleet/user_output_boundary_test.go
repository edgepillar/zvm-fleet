package fleet

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
)

func boundaryClaim(t *testing.T, e *Env) {
	t.Helper()
	e.Out = filepath.Join(t.TempDir(), "active")
	if err := e.StartUserRun(); err != nil {
		t.Fatal(err)
	}
}

func boundaryChange(t *testing.T, e *Env, kind string) string {
	t.Helper()
	retained := e.Out
	switch kind {
	case "directory", "symlink":
		retained = filepath.Join(filepath.Dir(e.Out), "retained")
		if err := os.Rename(e.Out, retained); err != nil {
			t.Fatal(err)
		}
		if kind == "directory" {
			if err := os.Mkdir(e.Out, 0o700); err != nil {
				t.Fatal(err)
			}
		} else {
			other := filepath.Join(filepath.Dir(e.Out), "other")
			if err := os.Mkdir(other, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, e.Out); err != nil {
				t.Skipf("directory symlink fixture unavailable: %v", err)
			}
		}
	case "marker-identity":
		path := filepath.Join(e.Out, userRunFile)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path, filepath.Join(e.Out, "retained-marker.json")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	case "marker-content":
		contextWriteJSON(t, e.Out, userRunFile, map[string]any{"schema": 1, "kind": "users", "runId": contextRunB})
	default:
		t.Fatalf("unknown boundary fixture: %s", kind)
	}
	return retained
}

func TestUserOutputPhaseBoundaryRefusesChangedClaim(t *testing.T) {
	for _, kind := range []string{"directory", "symlink", "marker-identity", "marker-content"} {
		t.Run(kind, func(t *testing.T) {
			e := settleFixtureEnv(t, settleFixtureOptions{})
			boundaryClaim(t, e)
			want := errors.New("synthetic workload error before output refusal")
			retained, nextRan := "", false
			err := e.RunPhases(context.Background(), []Phase{
				{ID: "00-boundary", Run: func(context.Context, *Env, *PhaseRecord) error {
					retained = boundaryChange(t, e, kind)
					return want
				}},
				{ID: "01-after-boundary", Run: func(context.Context, *Env, *PhaseRecord) error { nextRan = true; return nil }},
			})
			if !errors.Is(err, want) || nextRan || err.Error() == "phase 00-boundary: "+want.Error() {
				t.Error("changed claim lost the workload error, allowed the next phase, or omitted the output refusal")
			}
			for _, dir := range []string{e.Out, retained} {
				for _, name := range []string{"00-boundary.json", "01-after-boundary.json", "state.json"} {
					if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
						t.Errorf("changed claim wrote phase or state output: %s", name)
					}
				}
			}
		})
	}
}

func boundaryForeignState(t *testing.T, e *Env, kind string) string {
	t.Helper()
	path := filepath.Join(e.Out, "state.json")
	if kind == "symlink" {
		path = filepath.Join(t.TempDir(), "retained-state.json")
		if err := os.Symlink(path, filepath.Join(e.Out, "state.json")); err != nil {
			t.Skipf("state symlink fixture unavailable: %v", err)
		}
	}
	if err := os.WriteFile(path, []byte("retained synthetic state evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUserOutputStateRefusesUnexpectedFile(t *testing.T) {
	for _, kind := range []string{"regular", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			e := &Env{}
			boundaryClaim(t, e)
			path := boundaryForeignState(t, e, kind)
			if err := e.saveState(); err == nil {
				t.Error("unexpected state file was overwritten instead of refused")
			}
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != "retained synthetic state evidence\n" {
				t.Error("unexpected state evidence was changed")
			}
		})
	}
}

func TestUserOutputStateRefusesReplacementOrModification(t *testing.T) {
	for _, kind := range []string{"regular", "symlink", "content"} {
		t.Run(kind, func(t *testing.T) {
			e := &Env{}
			boundaryClaim(t, e)
			if err := e.saveState(); err != nil {
				t.Fatal(err)
			}
			if kind != "content" {
				if err := os.Rename(e.statePath(), filepath.Join(e.Out, "retained-state.json")); err != nil {
					t.Fatal(err)
				}
			}
			path := boundaryForeignState(t, e, kind)
			if err := e.saveState(); err == nil {
				t.Error("replaced or modified claimed state was overwritten instead of refused")
			}
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != "retained synthetic state evidence\n" {
				t.Error("replacement state evidence was changed")
			}
		})
	}
}

func TestUserOutputSampleBoundaryFailureIsSticky(t *testing.T) {
	for _, kind := range []string{"directory", "marker-identity", "marker-content"} {
		t.Run(kind, func(t *testing.T) {
			e := &Env{}
			boundaryClaim(t, e)
			closeSamples, err := e.StreamSamples()
			if err != nil {
				t.Fatal(err)
			}
			defer closeSamples()
			if err := e.onSample(Sample{T: 1000}); err != nil {
				t.Fatal(err)
			}
			retained := boundaryChange(t, e, kind)
			if e.onSample(Sample{T: 2000}) == nil || e.onSample(Sample{T: 3000}) == nil || closeSamples() == nil {
				t.Error("changed claim did not produce a sticky stream failure")
			}
			raw, err := os.ReadFile(filepath.Join(retained, "samples.jsonl"))
			if err != nil || bytes.Count(raw, []byte("\n")) != 1 || !bytes.Contains(raw, []byte(`"t":1000`)) {
				t.Error("changed claim appended past the accepted sample prefix")
			}
		})
	}
}

func TestUserOutputDirectSaveRefusesChangedClaim(t *testing.T) {
	for _, kind := range []string{"directory", "marker-identity", "marker-content"} {
		t.Run(kind, func(t *testing.T) {
			e := &Env{Rollup: &rollup.Config{}}
			boundaryClaim(t, e)
			boundaryChange(t, e, kind)
			if err := e.save(&PhaseRecord{ID: "00-direct"}); err == nil {
				t.Error("direct phase save ignored a changed claim")
			}
			if err := e.saveState(); err == nil {
				t.Error("direct state save ignored a changed claim")
			}
		})
	}
}
