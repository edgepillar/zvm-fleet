package fleet

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
)

func TestUserRecordWriteFailureDoesNotCount(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "closed-records.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	s := &sim{}
	s.out, s.w = f, bufio.NewWriterSize(f, 1)
	s.emit(&Record{T: 1000, Profile: "light", Action: "synthetic-tx", Kind: "tx", OK: true})
	if s.txs.Load() != 0 || s.fails.Load() != 0 {
		t.Fatal("failed record write was counted as a recorded transaction")
	}
}

func TestSampleEncodingFailurePreservesPrefix(t *testing.T) {
	e := &Env{Out: t.TempDir()}
	closeStream, err := e.StreamSamples()
	if err != nil {
		t.Fatal(err)
	}
	e.onSample(Sample{T: 1000})
	e.onSample(Sample{T: 1500, Relayer: map[string]any{"synthetic": math.NaN()}})
	e.onSample(Sample{T: 2000})
	closeStream()
	raw, err := os.ReadFile(filepath.Join(e.Out, "samples.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	if len(lines) != 1 || !bytes.Contains(lines[0], []byte(`"t":1000`)) {
		t.Fatal("sample encoding failure wrote a blank row or allowed later records")
	}
}

func TestRunPhasesReturnsStreamFailure(t *testing.T) {
	e := settleFixtureEnv(t, settleFixtureOptions{})
	closeStream, err := e.StreamSamples()
	if err != nil {
		t.Fatal(err)
	}
	defer closeStream()
	nextRan := false
	err = e.RunPhases(context.Background(), []Phase{
		{ID: "00-stream-failure", Run: func(context.Context, *Env, *PhaseRecord) error {
			e.onSample(Sample{T: 1000, Bridge: map[string]any{"synthetic": math.NaN()}})
			return nil
		}},
		{ID: "01-after-stream-failure", Run: func(context.Context, *Env, *PhaseRecord) error {
			nextRan = true
			return nil
		}},
	})
	if err == nil || nextRan {
		t.Error("sample stream failure returned success or allowed the next phase")
	}
	raw, readErr := os.ReadFile(filepath.Join(e.Out, "00-stream-failure.json"))
	if readErr != nil || !bytes.Contains(raw, []byte(`"error":`)) {
		t.Error("sample stream failure was absent from the retained phase")
	}
}

func TestRunUsersMetadataFailureStopsPreparation(t *testing.T) {
	e := &Env{Out: t.TempDir(), Rollup: &rollup.Config{}}
	if err := os.Mkdir(filepath.Join(e.Out, "users.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() != nil {
			t.Error("metadata write failure reached workload preparation")
		}
	}()
	p := &PhaseRecord{Data: map[string]any{}}
	if err := e.runUsers(context.Background(), p, UserOptions{}); err == nil {
		t.Error("metadata write failure returned success")
	}
}

func TestRunUsersPreservesExistingRecords(t *testing.T) {
	e := &Env{Out: t.TempDir(), Rollup: &rollup.Config{}}
	path := filepath.Join(e.Out, "records.jsonl")
	const retained = "retained synthetic record evidence\n"
	if err := os.WriteFile(path, []byte(retained), 0o600); err != nil {
		t.Fatal(err)
	}
	defer func() {
		panicked := recover() != nil
		raw, err := os.ReadFile(path)
		if panicked || err != nil || string(raw) != retained {
			t.Error("existing records were truncated or preparation began before refusal")
		}
	}()
	if err := e.runUsers(context.Background(), &PhaseRecord{Data: map[string]any{}}, UserOptions{}); err == nil {
		t.Error("existing records did not refuse a new simulation")
	}
}

func TestRunPhasesReturnsWorkloadFailure(t *testing.T) {
	e := settleFixtureEnv(t, settleFixtureOptions{})
	want := errors.New("synthetic workload failure")
	nextRan := false
	err := e.RunPhases(context.Background(), []Phase{
		{ID: "00-workload-failure", Run: func(context.Context, *Env, *PhaseRecord) error { return want }},
		{ID: "01-after-workload-failure", Run: func(context.Context, *Env, *PhaseRecord) error { nextRan = true; return nil }},
	})
	if !errors.Is(err, want) || !nextRan {
		t.Fatal("recorded workload failure was lost at the caller or changed continuation policy")
	}
}

func TestRunPhasesRetainsFailureWhenStateSaveFails(t *testing.T) {
	e := settleFixtureEnv(t, settleFixtureOptions{})
	if err := os.Mkdir(filepath.Join(e.Out, "state.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	want := errors.New("synthetic workload failure")
	err := e.RunPhases(context.Background(), []Phase{{ID: "00-state-failure", Run: func(context.Context, *Env, *PhaseRecord) error { return want }}})
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "state.json") {
		t.Fatal("state save failure replaced the original workload failure")
	}
}

func TestSampleStreamRejectsSecondActiveWriter(t *testing.T) {
	e := &Env{Out: t.TempDir()}
	first, err := e.StreamSamples()
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	second, err := e.StreamSamples()
	if second != nil {
		defer second()
	}
	if err == nil {
		t.Fatal("a second active sample writer replaced the owned stream")
	}
}
