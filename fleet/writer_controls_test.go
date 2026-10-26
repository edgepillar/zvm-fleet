package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Zenoglyphs-VM/zvm-bridge/bridge"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
)

type writerFaultOutput struct {
	bytes.Buffer
	writeErr, closeErr error
	count              *int
	writes             int
	closes             atomic.Int32
}

func (w *writerFaultOutput) Write(raw []byte) (int, error) {
	w.writes++
	if w.count != nil {
		return *w.count, w.writeErr
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.Buffer.Write(raw)
}

func (w *writerFaultOutput) Close() error { w.closes.Add(1); return w.closeErr }

func TestJSONLOutputRetainsEncodingFlushAndCloseFailures(t *testing.T) {
	writeErr, closeErr := errors.New("synthetic write failure"), errors.New("synthetic close failure")
	out := &writerFaultOutput{writeErr: writeErr, closeErr: closeErr}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	w := newJSONLOutput(out, cancel)
	if err := w.append(Sample{T: 1000}, false); err != nil {
		t.Fatal(err)
	}
	first := w.append(Sample{T: 1500, Relayer: map[string]any{"synthetic": math.NaN()}}, false)
	var encodingErr *json.UnsupportedValueError
	if !errors.As(first, &encodingErr) || context.Cause(ctx) != first {
		t.Fatal("encoding failure did not cancel its owner with the original cause")
	}
	if err := w.append(Sample{T: 2000}, true); err != first || out.writes != 0 {
		t.Fatal("a failed encoder attempted another row")
	}
	finished := w.finish()
	if !errors.Is(finished, first) || !errors.Is(finished, writeErr) || !errors.Is(finished, closeErr) {
		t.Fatal("finalization lost the encoding, flush or close failure")
	}
	if again := w.finish(); again != finished || out.closes.Load() != 1 || out.writes != 1 {
		t.Fatal("finalization retried output or closed it more than once")
	}
}

func TestJSONLOutputRejectsInvalidWriteCounts(t *testing.T) {
	for _, count := range []int{0, 1, -1, 1 << 20} {
		t.Run(string(rune('a'+count%26)), func(t *testing.T) {
			out := &writerFaultOutput{count: &count}
			w := newJSONLOutput(out, nil)
			err := w.append(map[string]any{"padding": strings.Repeat("x", 70<<10)}, false)
			if err == nil || out.writes != 1 {
				t.Fatal("invalid underlying write count was accepted or retried")
			}
			if count >= 0 && count < 70<<10 && !errors.Is(err, io.ErrShortWrite) {
				t.Fatal("short write without an underlying error lost ErrShortWrite")
			}
			if w.finish() == nil || out.closes.Load() != 1 {
				t.Fatal("write failure did not survive close")
			}
		})
	}
}

func TestJSONLOutputConcurrentFraming(t *testing.T) {
	out := new(writerFaultOutput)
	w := newJSONLOutput(out, nil)
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.append(Sample{T: int64(1000 + i)}, false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, line := range bytes.Split(bytes.TrimSuffix(out.Bytes(), []byte("\n")), []byte("\n")) {
		var row Sample
		if err := json.Unmarshal(line, &row); err != nil || seen[row.T] {
			t.Fatal("concurrent rows interleaved or duplicated")
		}
		seen[row.T] = true
	}
	if len(seen) != 64 || out.closes.Load() != 1 {
		t.Fatal("concurrent output lost rows or closed repeatedly")
	}
	if err := w.append(Sample{T: 2000}, true); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("closed output accepted another row")
	}
}

func TestSampleStreamCloseDuringPhaseStopsOwner(t *testing.T) {
	e := settleFixtureEnv(t, settleFixtureOptions{})
	closeStream, err := e.StreamSamples()
	if err != nil {
		t.Fatal(err)
	}
	nextRan := false
	err = e.RunPhases(context.Background(), []Phase{
		{ID: "00-early-close", Run: func(context.Context, *Env, *PhaseRecord) error { return closeStream() }},
		{ID: "01-after-close", Run: func(context.Context, *Env, *PhaseRecord) error { nextRan = true; return nil }},
	})
	if !errors.Is(err, io.ErrClosedPipe) || nextRan || !errors.Is(closeStream(), io.ErrClosedPipe) {
		t.Fatal("early close was confused with successful stream completion")
	}
}

func TestSampleStreamSuccessfulAppendAndReopen(t *testing.T) {
	e := &Env{Out: t.TempDir()}
	path := filepath.Join(e.Out, "samples.jsonl")
	if err := os.WriteFile(path, []byte("{\"t\":500}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, at := range []int64{1000, 2000} {
		closeStream, err := e.StreamSamples()
		if err != nil {
			t.Fatal(err)
		}
		if err := e.onSample(Sample{T: at}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil || !bytes.Contains(raw, []byte(`"t":`+big.NewInt(at).String())) {
			t.Fatal("sample was not flushed before stream close")
		}
		if closeStream() != nil || closeStream() != nil {
			t.Fatal("clean stream close was not idempotent")
		}
	}
	raw, _ := os.ReadFile(path)
	if bytes.Count(raw, []byte("\n")) != 3 || !bytes.HasPrefix(raw, []byte("{\"t\":500}\n")) {
		t.Fatal("append mode changed the existing prefix")
	}
}

func TestRunPhasesRefusesConcurrentConfiguration(t *testing.T) {
	e := settleFixtureEnv(t, settleFixtureOptions{})
	closeStream, err := e.StreamSamples()
	if err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- e.RunPhases(context.Background(), []Phase{{ID: "00-held", Run: func(context.Context, *Env, *PhaseRecord) error {
			close(entered)
			<-release
			return nil
		}}})
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("synthetic phase did not start")
	}
	if _, err := e.StreamSamples(); err == nil {
		t.Error("stream configuration changed during phase execution")
	}
	if err := e.RunPhases(context.Background(), nil); err == nil {
		t.Error("concurrent phase execution was accepted")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := closeStream(); err != nil {
		t.Fatal(err)
	}
}

func TestUserMetadataEncodingFailureBeforePreparation(t *testing.T) {
	e := &Env{Out: t.TempDir(), Rollup: &rollup.Config{}}
	err := e.runUsers(context.Background(), &PhaseRecord{Data: map[string]any{}}, UserOptions{Pace: math.NaN()})
	var encodingErr *json.UnsupportedValueError
	if !errors.As(err, &encodingErr) {
		t.Fatal("metadata encoding failure reached workload preparation")
	}
}

type trackedRecordOutput struct {
	*os.File
	closed atomic.Bool
}

func (o *trackedRecordOutput) Close() error { o.closed.Store(true); return o.File.Close() }

func writerSimulation(t *testing.T, duration time.Duration) (*sim, context.Context, context.CancelCauseFunc, *trackedRecordOutput) {
	t.Helper()
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "records.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	out := &trackedRecordOutput{File: f}
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil); f.Close() })
	s := &sim{e: &Env{Out: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		o: UserOptions{Duration: duration}, jsonlOutput: newJSONLOutput(out, cancel), followCtx: ctx}
	return s, ctx, cancel, out
}

func TestSimulationJoinsFollowupBeforeClosing(t *testing.T) {
	s, ctx, cancel, out := writerSimulation(t, 0)
	stopping, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		<-ctx.Done()
		close(stopping)
		<-release
		s.emit(&Record{T: 1000, Profile: "light", Action: "synthetic-followup", Kind: "payout", Error: "canceled"})
	}()
	go func() { done <- s.runSimulation(ctx, &PhaseRecord{Data: map[string]any{}}) }()
	want := errors.New("synthetic owner cancellation")
	cancel(want)
	<-stopping
	if out.closed.Load() {
		t.Error("record stream closed before its producer returned")
	}
	select {
	case <-done:
		t.Error("simulation returned before joining its producer")
	default:
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, want) || !out.closed.Load() {
			t.Fatal("joined simulation lost cancellation or did not close output")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled simulation failed to drain")
	}
	raw, err := os.ReadFile(filepath.Join(s.e.Out, "records.jsonl"))
	if err != nil || !bytes.Contains(raw, []byte("synthetic-followup")) || bytes.Count(raw, []byte("\n")) != 1 {
		t.Fatal("last producer's complete record was lost")
	}
}

func TestSimulationStopsOnRecordFailure(t *testing.T) {
	s, ctx, _, _ := writerSimulation(t, time.Hour)
	want := errors.New("synthetic output failure")
	fault := &writerFaultOutput{writeErr: want}
	s.jsonlOutput = newJSONLOutput(fault, s.cancel)
	s.pending.Add(1)
	go func() {
		defer s.pending.Done()
		s.emit(&Record{T: 1000, Profile: "light", Action: "synthetic-tx", Kind: "tx", Error: strings.Repeat("x", 70<<10)})
	}()
	done := make(chan error, 1)
	go func() { done <- s.runSimulation(ctx, &PhaseRecord{Data: map[string]any{}}) }()
	select {
	case err := <-done:
		if !errors.Is(err, want) || s.txs.Load() != 0 || fault.closes.Load() != 1 {
			t.Fatal("record failure did not cancel, drain and fail the simulation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("record failure left the simulation running")
	}
}

func TestSimulationSummaryFailureReachesCaller(t *testing.T) {
	s, ctx, _, out := writerSimulation(t, 0)
	if err := s.runSimulation(ctx, &PhaseRecord{Data: map[string]any{}}); err == nil || !out.closed.Load() {
		t.Fatal("empty record summary was reported as a successful simulation")
	}
}

func TestSimulationActionDeadlineKeepsOwnedFollowup(t *testing.T) {
	s, ctx, _, out := writerSimulation(t, 0)
	p := &PhaseRecord{Data: map[string]any{}}
	release, done := make(chan struct{}), make(chan error, 1)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		<-release
		s.emit(&Record{T: 1000, Profile: "light", Action: "synthetic-late-followup", Kind: "payout", OK: true})
	}()
	go func() { done <- s.runSimulation(ctx, p) }()
	deadline, tick := time.NewTimer(2*time.Second), time.NewTicker(5*time.Millisecond)
	defer deadline.Stop()
	defer tick.Stop()
	for {
		p.mu.Lock()
		_, actionsEnded := p.Data["simEnd"]
		p.mu.Unlock()
		if actionsEnded {
			break
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			close(release)
			t.Fatal("synthetic action deadline did not end")
		}
	}
	if ctx.Err() != nil || out.closed.Load() {
		t.Error("normal action deadline canceled the run or closed output before its followup")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil || !out.closed.Load() {
			t.Fatal("normal action deadline lost a completed followup or summary")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("normal followup did not drain")
	}
}

func TestWithdrawalObservationUsesRunCancellation(t *testing.T) {
	e := settleFixtureEnv(t, settleFixtureOptions{})
	s, ctx, cancel, _ := writerSimulation(t, 0)
	s.e.Eth = e.Eth
	s.blockTime.Store(uint64(1), int64(1))
	u := &user{s: s, p: Profiles[0]}
	cancel(errors.New("synthetic observation stop"))
	u.followWithdrawal(ctx, &types.Receipt{BlockNumber: big.NewInt(1), Logs: []*types.Log{{
		Address: rollup.BridgeAddr, Topics: []common.Hash{bridge.TopicWithdrawalInitiated, common.BigToHash(big.NewInt(1)), {}, {}},
	}}})
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("withdrawal observation detached from run cancellation")
	}
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.e.Out, "records.jsonl"))
	if err != nil || !bytes.Contains(raw, []byte("payout observation canceled")) {
		t.Fatal("canceled payout observation omitted its terminal record")
	}
}
