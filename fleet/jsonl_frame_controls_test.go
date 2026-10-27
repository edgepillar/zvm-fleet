package fleet

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONLFrameExactReaderBoundariesReplay(t *testing.T) {
	for _, name := range []string{"bytes", "depth", "tokens"} {
		t.Run(name, func(t *testing.T) {
			var value json.RawMessage
			switch name {
			case "bytes":
				prefix, suffix := `{"t":1000,"extension":"`, `"}`
				value = json.RawMessage(prefix + strings.Repeat("x", userJSONLLineLimit-len(prefix)-len(suffix)) + suffix)
			case "depth":
				value = json.RawMessage(`{"t":1000,"extension":` + strings.Repeat("[", userJSONLDepthLimit-1) + `0` + strings.Repeat("]", userJSONLDepthLimit-1) + `}`)
			case "tokens":
				// Root, two keys, scalar t, array opening/closing and root
				// closing consume seven tokens; every zero consumes one.
				count := userJSONLTokenLimit - 7
				value = json.RawMessage(`{"t":1000,"extension":[` + strings.Repeat("0,", count-1) + `0]}`)
			}
			canonical, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if name == "bytes" && len(canonical) != userJSONLLineLimit {
				t.Fatal("fixture missed the exact encoded byte limit")
			}
			out := &writerFaultOutput{}
			stream := newJSONLOutput(out, nil)
			if err := stream.append(value, true); err != nil {
				t.Fatal("reader-boundary row was rejected", err)
			}
			if err := stream.finish(); err != nil {
				t.Fatal(err)
			}
			wire := out.Bytes()
			if len(wire) != len(canonical)+1 || wire[len(wire)-1] != '\n' || !bytes.Equal(wire[:len(canonical)], canonical) {
				t.Fatal("frame validation changed encoded bytes")
			}
			evidence, err := scanUserJSONL(bytes.NewReader(wire), "samples.jsonl", func(raw []byte) error { _, err := userSample(raw); return err })
			if err != nil || evidence.Rows != 1 || evidence.State != "terminated" || evidence.Bytes != int64(len(wire)) {
				t.Fatal("producer boundary row could not replay through the real sample reader", err)
			}
		})
	}
}

func TestJSONLFrameRecordPreservesMeasuredZerosAndUnknowns(t *testing.T) {
	for _, known := range []bool{false, true} {
		record := Record{T: 1000, Profile: "light", Action: "synthetic-tx", Kind: "tx", OK: true,
			GasKnown: &known, ConfirmKnown: &known, IncludeKnown: &known, LatencyKnown: &known}
		canonical, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		out := &writerFaultOutput{}
		stream := newJSONLOutput(out, nil)
		if err := stream.append(&record, true); err != nil {
			t.Fatal(err)
		}
		if err := stream.finish(); err != nil {
			t.Fatal(err)
		}
		raw := bytes.TrimSuffix(out.Bytes(), []byte("\n"))
		replay, err := userRecord(raw)
		if err != nil || !bytes.Equal(raw, canonical) || replay.GasKnown == nil || *replay.GasKnown != known ||
			replay.ConfirmKnown == nil || *replay.ConfirmKnown != known || replay.IncludeKnown == nil || *replay.IncludeKnown != known ||
			replay.LatencyKnown == nil || *replay.LatencyKnown != known {
			t.Fatal("frame validation changed bytes or zero/unknown availability", err)
		}
	}
}
