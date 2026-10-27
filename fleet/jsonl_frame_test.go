package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Large negative rows are hashed and discarded, not retained in a second
// buffer or written to the filesystem by the fixture sink.
type jsonlFrameOutput struct {
	digest         hash.Hash
	bytes          int64
	writes, closes int
}

func newJSONLFrameOutput() *jsonlFrameOutput { return &jsonlFrameOutput{digest: sha256.New()} }
func (o *jsonlFrameOutput) Write(raw []byte) (int, error) {
	o.writes++
	n, err := o.digest.Write(raw)
	o.bytes += int64(n)
	return n, err
}
func (o *jsonlFrameOutput) Close() error { o.closes++; return nil }

func jsonlFrameDeepSample() Sample {
	var nested any = 0
	for range userJSONLDepthLimit {
		nested = []any{nested}
	}
	return Sample{T: 2000, Relayer: map[string]any{"nested": nested}}
}

func jsonlFrameLargeRecord() *Record {
	return &Record{T: 2000, Profile: "light", Action: "synthetic-read", Kind: "read",
		Error: strings.Repeat("x", userJSONLLineLimit+1)}
}

func jsonlFrameBadCases() []struct {
	name  string
	value func() any
} {
	return []struct {
		name  string
		value func() any
	}{
		{"null", func() any { return (*Sample)(nil) }},
		{"array", func() any { return json.RawMessage(`[{"t":2000}]`) }},
		{"string", func() any { return "synthetic-sensitive-body" }},
		{"boolean", func() any { return true }},
		{"number", func() any { return 42 }},
		{"duplicate", func() any { return json.RawMessage(`{"t":2000,"t":3000}`) }},
		{"nested-duplicate", func() any {
			return Sample{T: 2000, Relayer: map[string]any{"nested": json.RawMessage(`{"x":1,"x":2}`)}}
		}},
		{"depth", func() any { return jsonlFrameDeepSample() }},
		{"tokens", func() any {
			return json.RawMessage(`{"t":2000,"extension":[` + strings.Repeat("0,", userJSONLTokenLimit) + `0]}`)
		}},
		{"record-bytes", func() any { return jsonlFrameLargeRecord() }},
		{"sample-escaped-bytes", func() any {
			return Sample{T: 2000, Relayer: map[string]any{"message": strings.Repeat("<", userJSONLLineLimit/6+1)}}
		}},
	}
}

func TestJSONLOutputRefusesReaderIncompatibleFrame(t *testing.T) {
	for _, tc := range jsonlFrameBadCases() {
		t.Run(tc.name, func(t *testing.T) {
			value := tc.value()
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal("fixture must have valid standard JSON encoding", err)
			}
			if len(raw) <= userJSONLLineLimit && userJSONObject(raw) == nil {
				t.Fatal("fixture is not reader-incompatible")
			}
			out := newJSONLFrameOutput()
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			stream := newJSONLOutput(out, cancel)
			prefix := Sample{T: 1000}
			expected, err := json.Marshal(prefix)
			if err != nil {
				t.Fatal(err)
			}
			expected = append(expected, '\n')
			want := sha256.Sum256(expected)
			if err := stream.append(prefix, false); err != nil {
				t.Fatal(err)
			}
			first := stream.append(value, false)
			if first == nil || context.Cause(ctx) == nil {
				t.Error("reader-incompatible row was accepted by producer")
			}
			if first != nil && (len(first.Error()) > 512 || strings.Contains(first.Error(), "synthetic-sensitive-body")) {
				t.Error("frame refusal exposed serialized row content")
			}
			if out.writes != 0 {
				t.Error("rejected row reached the output before finalization")
			}
			if again := stream.append(Sample{T: 3000}, true); again == nil || first != nil && !errors.Is(again, first) {
				t.Error("frame failure did not stop later rows")
			}
			if err := stream.finish(); err == nil || first != nil && !errors.Is(err, first) {
				t.Error("frame failure was lost during finalization")
			}
			if out.bytes != int64(len(expected)) || !bytes.Equal(out.digest.Sum(nil), want[:]) || out.closes != 1 {
				t.Error("frame refusal did not retain exactly the earlier accepted prefix")
			}
		})
	}
}

func TestJSONLRecordEmitterStopsBeforeCountingUnreadableFrame(t *testing.T) {
	out := newJSONLFrameOutput()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	s := &sim{e: &Env{}, jsonlOutput: newJSONLOutput(out, cancel)}
	row := jsonlFrameLargeRecord()
	row.Kind, row.OK = "tx", true
	s.emit(row)
	if context.Cause(ctx) == nil || s.txs.Load() != 0 || s.fails.Load() != 0 {
		t.Error("unreadable record was accepted or counted by simulation")
	}
	if err := s.finish(); err == nil {
		t.Error("unreadable record was reported as clean output")
	}
	if out.bytes != 0 || out.closes != 1 {
		t.Error("record refusal persisted an unreadable row")
	}
}

func TestJSONLSampleEmitterStopsBeforeUnreadableFrame(t *testing.T) {
	e := &Env{}
	boundaryClaim(t, e)
	closeStream, err := e.StreamSamples()
	if err != nil {
		t.Fatal(err)
	}
	defer closeStream()
	if err := e.onSample(Sample{T: 1000}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.Out, "samples.jsonl")
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	e.stream.bindPhase(cancel)
	first := e.onSample(jsonlFrameDeepSample())
	if first == nil || context.Cause(ctx) == nil {
		t.Error("unreadable sample was accepted without stopping its owner")
	}
	e.stream.bindPhase(nil)
	if err := closeStream(); err == nil {
		t.Error("sample finalization lost its frame refusal")
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, expected) {
		t.Error("sample refusal changed the accepted prefix")
	}
}
