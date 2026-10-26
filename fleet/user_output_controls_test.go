package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUserOutputStateUpdatesRetainOwnedFile(t *testing.T) {
	e := &Env{}
	boundaryClaim(t, e)
	e.owned = map[int][]*big.Int{1: {big.NewInt(123456789), big.NewInt(987654321)}}
	if err := e.saveState(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(e.statePath())
	if err != nil {
		t.Fatal(err)
	}
	e.state.Onboarded = 2
	e.owned[2] = []*big.Int{big.NewInt(123)}
	if err := e.saveState(); err != nil {
		t.Fatal(err)
	}
	e.state.Onboarded, e.owned = 0, nil
	if err := e.saveState(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(e.statePath())
	var state State
	after, statErr := os.Lstat(e.statePath())
	if err != nil || statErr != nil || json.Unmarshal(raw, &state) != nil || state.Onboarded != 0 || len(state.Owned) != 0 ||
		!os.SameFile(before, after) || after.Size() >= before.Size() {
		t.Fatal("owned state updates changed identity, lost data or retained a stale suffix")
	}
	if err := e.saveState(); err != nil {
		t.Fatal("unchanged owned state could not be saved again")
	}
}

func TestUserOutputStateDetectsSameSizeBytesWithRestoredTime(t *testing.T) {
	e := &Env{}
	boundaryClaim(t, e)
	if err := e.saveState(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(e.statePath())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(e.statePath())
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(raw, []byte(`"nativeOnboarded": 0`), []byte(`"nativeOnboarded": 1`), 1)
	if bytes.Equal(raw, changed) || len(raw) != len(changed) {
		t.Fatal("invalid same-size state fixture")
	}
	if err := os.WriteFile(e.statePath(), changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(e.statePath(), before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := e.saveState(); err == nil {
		t.Fatal("same-size state modification with restored time was overwritten")
	}
	after, err := os.ReadFile(e.statePath())
	if err != nil || !bytes.Equal(changed, after) {
		t.Fatal("refused modified state was changed")
	}
}

func TestUserOutputOpenedStreamRefusesReplacement(t *testing.T) {
	e := &Env{}
	boundaryClaim(t, e)
	f, err := e.openUserOutput("records.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "retained synthetic prefix\n"
	if _, err := f.Write([]byte(prefix)); err != nil {
		t.Fatal(err)
	}
	retained := boundaryChange(t, e, "directory")
	if n, err := f.Write([]byte("later bytes\n")); n != 0 || err == nil {
		t.Error("opened stream wrote after observing a replacement directory")
	}
	if err := f.Close(); err == nil {
		t.Error("stream close omitted the changed claim")
	}
	raw, err := os.ReadFile(filepath.Join(retained, "records.jsonl"))
	if err != nil || string(raw) != prefix {
		t.Error("original stream prefix was changed")
	}
	if _, err := os.Lstat(filepath.Join(e.Out, "records.jsonl")); !os.IsNotExist(err) {
		t.Error("stream created output in the replacement directory")
	}
}

func TestUserOutputRecordFlushCancelsOnClaimChange(t *testing.T) {
	e := &Env{}
	boundaryClaim(t, e)
	f, err := e.openUserOutput("records.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	s := &sim{e: e, jsonlOutput: newJSONLOutput(f, cancel)}
	defer s.finish()
	s.emit(&Record{T: 1000, Profile: "light", Kind: "read", Action: "synthetic-read", OK: true})
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	retained := boundaryChange(t, e, "marker-content")
	s.emit(&Record{T: 2000, Profile: "light", Kind: "read", Action: "synthetic-read", OK: true})
	if s.flush() == nil || context.Cause(ctx) == nil || s.finish() == nil {
		t.Fatal("record flush lost the changed claim or did not cancel its simulation")
	}
	raw, err := os.ReadFile(filepath.Join(retained, "records.jsonl"))
	if err != nil || bytes.Count(raw, []byte("\n")) != 1 || !bytes.Contains(raw, []byte(`"t":1000`)) {
		t.Fatal("record failure lost the written prefix or wrote the later buffered row")
	}
}

func TestUserOutputRootStaysBoundAcrossDirectoryRename(t *testing.T) {
	e := &Env{}
	boundaryClaim(t, e)
	r, err := e.openUserRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	retained := boundaryChange(t, e, "directory")
	// Exercise the descriptor's binding directly, independently of the refusal
	// checks: an open/create race must still not address the replacement path.
	f, err := r.OpenFile("synthetic-anchor.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeUserBytes(f, []byte(`{"synthetic":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(retained, "synthetic-anchor.json")); err != nil {
		t.Fatal("root descriptor did not retain the original directory")
	}
	if _, err := os.Lstat(filepath.Join(e.Out, "synthetic-anchor.json")); !os.IsNotExist(err) {
		t.Fatal("root descriptor followed the replacement directory")
	}
}

func TestUserOutputInitRefusesUnexpectedStateBeforeProbe(t *testing.T) {
	calls := resourceDocker(t, "")
	e := &Env{Probe: "synthetic-probe"}
	boundaryClaim(t, e)
	path := boundaryForeignState(t, e, "regular")
	if err := e.Init(context.Background()); err == nil || !strings.Contains(err.Error(), "state.json") {
		t.Fatal("initialization accepted unexpected claimed state")
	}
	if _, err := os.Lstat(calls); !os.IsNotExist(err) {
		t.Fatal("unexpected initial state reached the Docker fixture")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "retained synthetic state evidence\n" {
		t.Fatal("initialization changed unexpected state evidence")
	}
}

func TestUserOutputInitWritesClaimedProfile(t *testing.T) {
	resourceDocker(t, "")
	e := &Env{Probe: "synthetic-probe"}
	boundaryClaim(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Init(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(e.Out, "names.json"))
	var names Names
	if err != nil || json.Unmarshal(raw, &names) != nil || names.RunID != e.userRunToken() {
		t.Fatal("initialization omitted the claimed profile declaration")
	}
	if _, err := os.Stat(filepath.Join(e.Out, "runs")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(e.statePath()); !os.IsNotExist(err) {
		t.Fatal("initialization adopted or invented user state")
	}
}
