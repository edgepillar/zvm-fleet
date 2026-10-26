package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
)

func TestUserRunClaimPreservesExistingOutput(t *testing.T) {
	for _, name := range []string{"records.jsonl", "samples.jsonl", "users.json", "names.json", "state.json", "00-users-old.json", "unrelated.txt"} {
		t.Run(name, func(t *testing.T) {
			e := &Env{Out: t.TempDir()}
			path := filepath.Join(e.Out, name)
			const retained = "retained synthetic evidence\n"
			if err := os.WriteFile(path, []byte(retained), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := e.StartUserRun(); err == nil {
				t.Fatal("nonempty user output was claimed")
			}
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != retained {
				t.Fatal("claim changed prior evidence")
			}
			if _, err := os.Lstat(filepath.Join(e.Out, userRunFile)); !os.IsNotExist(err) {
				t.Fatal("refused output acquired a new marker")
			}
		})
	}
}

func TestUserRunClaimSerializesCooperatingWriters(t *testing.T) {
	dir := t.TempDir()
	gate := make(chan struct{})
	envs := []*Env{{Out: dir}, {Out: dir}}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, e := range envs {
		wg.Add(1)
		go func() { defer wg.Done(); <-gate; results <- e.StartUserRun() }()
	}
	close(gate)
	wg.Wait()
	success := 0
	for range 2 {
		if <-results == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatal("exclusive marker did not select exactly one cooperating owner")
	}
	for _, e := range envs {
		if e.userRun != nil && (!validUserRunID(e.userRunToken()) || e.checkUserRun() != nil) {
			t.Fatal("winning claim lost its identity")
		}
	}
	other := &Env{Out: t.TempDir()}
	if other.StartUserRun() != nil {
		t.Fatal("independent output could not be claimed")
	}
	for _, e := range envs {
		if e.userRun != nil && e.userRunToken() == other.userRunToken() {
			t.Fatal("separate runs reused a fixed identifier")
		}
	}
}

func TestUserRunMarkerMutationStopsBeforePhase(t *testing.T) {
	e := &Env{Out: t.TempDir()}
	if err := e.StartUserRun(); err != nil {
		t.Fatal(err)
	}
	contextWriteJSON(t, e.Out, userRunFile, map[string]any{"schema": 1, "kind": "users", "runId": contextRunB})
	ran := false
	if err := e.RunPhases(context.Background(), []Phase{{ID: "00-synthetic", Run: func(context.Context, *Env, *PhaseRecord) error { ran = true; return nil }}}); err == nil || ran {
		t.Fatal("changed marker allowed a phase or probe to start")
	}
	if _, err := e.StreamSamples(); err == nil {
		t.Fatal("changed marker allowed sample output")
	}
}

func TestUserRunDirectoryReplacementIsRefused(t *testing.T) {
	parent := t.TempDir()
	e := &Env{Out: filepath.Join(parent, "active")}
	if err := e.StartUserRun(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(e.Out, filepath.Join(parent, "retained")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(e.Out, 0o700); err != nil {
		t.Fatal(err)
	}
	if e.checkUserRun() == nil {
		t.Fatal("replacement directory reused the old claim")
	}
	if _, err := os.Stat(filepath.Join(parent, "retained", userRunFile)); err != nil {
		t.Fatal("refusal lost the original marker")
	}
}

func TestUserRunPhaseOutputAndIDsAreCheckedBeforeProbing(t *testing.T) {
	for _, name := range []string{"existing", "duplicate", "nonlocal"} {
		t.Run(name, func(t *testing.T) {
			e := &Env{Out: t.TempDir()}
			if err := e.StartUserRun(); err != nil {
				t.Fatal(err)
			}
			ran := false
			phase := Phase{ID: "00-synthetic", Run: func(context.Context, *Env, *PhaseRecord) error { ran = true; return nil }}
			phases := []Phase{phase}
			switch name {
			case "existing":
				contextWriteJSON(t, e.Out, "00-synthetic.json", map[string]any{"retained": true})
			case "duplicate":
				phases = append(phases, phase)
			case "nonlocal":
				phases[0].ID = "../outside"
			}
			if err := e.RunPhases(context.Background(), phases); err == nil || ran {
				t.Fatal("invalid phase output reached a phase or probe")
			}
		})
	}
}

func TestUserRunPhaseRetainsItsDeclaration(t *testing.T) {
	e := settleFixtureEnv(t, settleFixtureOptions{})
	if err := e.StartUserRun(); err != nil {
		t.Fatal(err)
	}
	if err := e.RunPhases(context.Background(), []Phase{{ID: "00-synthetic", Run: func(context.Context, *Env, *PhaseRecord) error { return nil }}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(e.Out, "00-synthetic.json"))
	var phase PhaseRecord
	if err != nil || json.Unmarshal(raw, &phase) != nil || phase.RunID != e.userRunToken() {
		t.Fatal("phase output omitted the run declaration")
	}
}

func TestUserRunWritersCarryOneDeclaration(t *testing.T) {
	e := &Env{Out: t.TempDir()}
	if err := e.StartUserRun(); err != nil {
		t.Fatal(err)
	}
	closeSamples, err := e.StreamSamples()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.onSample(Sample{T: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := closeSamples(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.StreamSamples(); err == nil {
		t.Fatal("claimed sample output was reopened for append")
	}
	out := new(writerFaultOutput)
	s := &sim{e: e, jsonlOutput: newJSONLOutput(out, nil)}
	s.emit(&Record{T: 1000, Profile: "light", Kind: "read", Action: "synthetic-read", OK: true})
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	var record Record
	if json.Unmarshal(bytes.TrimSpace(out.Bytes()), &record) != nil || record.RunID != e.userRunToken() {
		t.Fatal("record writer omitted the run declaration")
	}
	raw, err := os.ReadFile(filepath.Join(e.Out, "samples.jsonl"))
	var sample Sample
	if err != nil || json.Unmarshal(bytes.TrimSpace(raw), &sample) != nil || sample.RunID != record.RunID {
		t.Fatal("sample writer omitted the shared run declaration")
	}
}

func TestUserRunMetadataWriteFailureStopsPreparation(t *testing.T) {
	e := &Env{Out: t.TempDir(), Rollup: &rollup.Config{}}
	if err := e.StartUserRun(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(e.Out, "users.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := e.runUsers(context.Background(), &PhaseRecord{Data: map[string]any{}}, UserOptions{})
	if err == nil || !strings.Contains(err.Error(), "writing user metadata") {
		t.Fatal("claimed metadata failure reached workload preparation")
	}
}

func declaredContextFixture(t *testing.T) string {
	t.Helper()
	dir := contextLegacyFixture(t)
	meta := contextMetadata()
	meta["runId"] = contextRunA
	phase := contextPhase(meta)
	phase["runId"] = contextRunA
	phase["data"].(map[string]any)["simStart"] = "1970-01-01T00:00:01Z"
	phase["data"].(map[string]any)["simEnd"] = "1970-01-01T00:00:11Z"
	raw, _ := json.Marshal(V1Names())
	var names map[string]any
	if err := json.Unmarshal(raw, &names); err != nil {
		t.Fatal(err)
	}
	names["runId"] = contextRunA
	for file, value := range map[string]any{"users.json": meta, "00-users-synthetic.json": phase, "names.json": names,
		userRunFile: map[string]any{"schema": 1, "kind": "users", "runId": contextRunA}} {
		contextWriteJSON(t, dir, file, value)
	}
	writeSampleAccountingLines(t, filepath.Join(dir, "records.jsonl"), []map[string]any{
		{"t": 1000, "user": 0, "profile": "light", "kind": "tx", "action": "synthetic-tx", "ok": true, "runId": contextRunA},
		{"t": 2000, "user": 0, "profile": "light", "kind": "tx", "action": "synthetic-tx", "ok": true, "runId": contextRunA},
	})
	writeSampleAccountingLines(t, filepath.Join(dir, "samples.jsonl"), []map[string]any{
		{"t": 1000, "l1": 1, "l2": 1, "runId": contextRunA}, {"t": 11000, "l1": 3, "l2": 3, "runId": contextRunA},
		{"t": 12000, "l1": 99, "l2": 99, "runId": contextRunA},
	})
	return dir
}

func TestUserContextAcceptsConsistentDeclarationsAndClipsSamples(t *testing.T) {
	dir := declaredContextFixture(t)
	before, _ := os.ReadFile(filepath.Join(dir, "samples.jsonl"))
	s, err := SummarizeUsers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Inputs.Cohort.State != "declared-consistent" || !s.Inputs.Cohort.ContextComplete || s.Inputs.Cohort.RunID != contextRunA || s.Inputs.RunCompletion != "not-established" {
		t.Fatal("consistent declarations lost their evidence boundary")
	}
	if s.Inputs.Samples.Rows != 3 || s.Inputs.Samples.UsedRows != 2 || len(s.Windows) != 1 || s.Windows[0].L2Blocks != 2 || !s.Windows[0].L2BlocksKnown {
		t.Fatal("out-of-interval samples contaminated declared-window counters")
	}
	if !s.Totals.RateKnown || !s.Totals.PeakRateKnown || s.Totals.RateBasis != "declared-simulation" || math.Abs(s.Totals.TxPerSec-0.2) > 1e-12 {
		t.Fatal("declared interval lost its rate basis")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "samples.jsonl"))
	if !bytes.Equal(before, after) {
		t.Fatal("sample clipping rewrote the retained input")
	}
}

func TestUserContextRejectsAmbiguousIdentifierFields(t *testing.T) {
	for _, field := range []string{`"runId":null`, `"runId":""`, `"runId":1`, `"runId":"ABCDEF00000000000000000000000000"`, `"RUNID":"` + contextRunA + `"`, `"runId":"` + contextRunA + `","RunId":"` + contextRunA + `"`} {
		t.Run(field, func(t *testing.T) {
			dir := declaredContextFixture(t)
			raw := `{"t":1000,"profile":"light","kind":"read","action":"synthetic-read",` + field + "}\n"
			if err := os.WriteFile(filepath.Join(dir, "records.jsonl"), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if s, err := SummarizeUsers(dir); err == nil || s != nil {
				t.Fatal("ambiguous identifier field supplied a run binding")
			}
		})
	}
}

func TestUserContextZeroDurationHasNoRate(t *testing.T) {
	dir := userJSONLFixture(t, `{"t":1000,"profile":"light","kind":"tx","action":"synthetic-tx","ok":true}`+"\n", userJSONLSampleA+"\n")
	s, err := SummarizeUsers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Totals.RateKnown || s.Totals.PeakRateKnown || len(s.Windows) != 1 || s.Windows[0].RateKnown || s.Profiles[0].PerHourKnown {
		t.Fatal("zero duration became a measured rate")
	}
	if !strings.Contains(UsersMarkdown(s), "| Mean offered tx/s | n/a |") {
		t.Fatal("unknown duration rendered a numeric offered rate")
	}
}

func TestUserContextOpenIntervalStaysPartial(t *testing.T) {
	dir := contextLegacyFixture(t)
	phase := contextPhase(contextMetadata())
	phase["data"].(map[string]any)["simStart"] = "1970-01-01T00:00:01Z"
	contextWriteJSON(t, dir, "00-users-synthetic.json", phase)
	s, err := SummarizeUsers(dir)
	if err != nil || !s.Inputs.Partial || s.Inputs.Temporal.Basis != "declared-start-retained-prefix" {
		t.Fatal("unfinished interval lost its explicit prefix basis")
	}
}

func TestUserContextFinalEndpointAndWindowDenominator(t *testing.T) {
	dir := contextLegacyFixture(t)
	phase := contextPhase(contextMetadata())
	data := phase["data"].(map[string]any)
	data["simStart"], data["simEnd"] = "1970-01-01T00:00:59Z", "1970-01-01T00:30:00Z"
	contextWriteJSON(t, dir, "00-users-synthetic.json", phase)
	writeSampleAccountingLines(t, filepath.Join(dir, "records.jsonl"), []map[string]any{
		{"t": 59000, "profile": "light", "kind": "tx", "action": "synthetic-tx"}, {"t": 1799000, "profile": "light", "kind": "tx", "action": "synthetic-tx"},
	})
	writeSampleAccountingLines(t, filepath.Join(dir, "samples.jsonl"), []map[string]any{{"t": 59000, "l2": 10}, {"t": 1800000, "l2": 20}})
	s, err := SummarizeUsers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Windows) != 1 || !s.Windows[0].L2BlocksKnown || s.Windows[0].L2Blocks != 10 || math.Abs(s.Windows[0].TxPerSec-2.0/1741) > 1e-12 {
		t.Fatal("terminal endpoint or padded window diluted the observed interval")
	}
}

func TestUserContextPayoutClockIsNotAnActionStart(t *testing.T) {
	dir := contextLegacyFixture(t)
	phase := contextPhase(contextMetadata())
	data := phase["data"].(map[string]any)
	data["simStart"], data["simEnd"] = "1970-01-01T00:00:10Z", "1970-01-01T00:00:11Z"
	contextWriteJSON(t, dir, "00-users-synthetic.json", phase)
	writeSampleAccountingLines(t, filepath.Join(dir, "records.jsonl"), []map[string]any{
		{"t": 10000, "profile": "light", "kind": "tx", "action": "synthetic-tx"},
		{"t": 5000, "profile": "light", "kind": "payout", "action": "synthetic-payout"},
	})
	if s, err := SummarizeUsers(dir); err != nil || s.Totals.Payouts != 1 || !s.Start.Equal(time.UnixMilli(10000)) {
		t.Fatal("payout block anchor was treated as a wall-clock action")
	}
}

func TestUserContextRejectsDuplicateMetadataKeys(t *testing.T) {
	dir := contextLegacyFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "users.json"), []byte(`{"label":"first","label":"second"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := SummarizeUsers(dir); err == nil || s != nil {
		t.Fatal("duplicate metadata key changed the selected context")
	}
}

func TestUserContextPresentSparseProfileKeepsOnlyDeclaredNames(t *testing.T) {
	dir := contextLegacyFixture(t)
	contextWriteJSON(t, dir, "names.json", map[string]any{"Node": "synthetic-node"})
	s := &UsersSummary{}
	if err := loadUserContext(dir, s); err != nil {
		t.Fatal(err)
	}
	if s.context.names.Node != "synthetic-node" || s.context.names.Relayer != "" || len(s.context.names.EngineCandidates) != 0 {
		t.Fatal("present sparse profile inherited absent-file defaults")
	}
}

func TestUserContextPhaseOnlyPopulationStillChecksRoster(t *testing.T) {
	dir := contextLegacyFixture(t)
	if err := os.Remove(filepath.Join(dir, "users.json")); err != nil {
		t.Fatal(err)
	}
	writeSampleAccountingLines(t, filepath.Join(dir, "records.jsonl"), []map[string]any{{"t": 1000, "user": 1, "profile": "light", "kind": "read", "action": "synthetic-read"}})
	if s, err := SummarizeUsers(dir); err == nil || s != nil {
		t.Fatal("phase-only declared roster was ignored")
	}
}
