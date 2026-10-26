package fleet

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestResourceDecodedFieldsKeepPresenceAndDeclaredAvailability(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		present map[string]bool
	}{
		{`{}`, map[string]bool{}},
		{`{"cpu":0,"mem":0,"blkWrite":0}`, map[string]bool{"cpu": true, "mem": true, "blkWrite": true}},
		{`{"cpu":null,"mem":null,"blkWrite":null}`, map[string]bool{}},
		{`{"cpu":25,"mem":100,"known":{"cpu":false,"mem":true}}`, map[string]bool{"mem": true}},
		{`{"known":{"cpu":true,"mem":true,"blkWrite":true}}`, map[string]bool{}},
		{`{"cpu":-1,"mem":-1,"blkWrite":18446744073709551615}`, map[string]bool{}},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			var stat ContainerStat
			if err := decodeMeasurementJSON(strings.NewReader(tc.raw), &stat); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"cpu", "mem", "netRx", "netTx", "blkRead", "blkWrite"} {
				if stat.Known == nil || stat.Known[key] != tc.present[key] {
					t.Fatalf("field %s did not preserve individual availability", key)
				}
			}
			raw, err := json.Marshal(stat)
			if err != nil {
				t.Fatal(err)
			}
			var decoded ContainerStat
			if err := decodeMeasurementJSON(bytes.NewReader(raw), &decoded); err != nil {
				t.Fatal(err)
			}
			for key, want := range stat.Known {
				if decoded.Known[key] != want {
					t.Fatal("resource persistence changed availability")
				}
			}
		})
	}
}

func TestResourceDecimalSizeLimits(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		want  int64
		known bool
	}{
		{"0B", 0, true}, {"1.5MiB", 1572864, true}, {"1.234kB", 1234, true}, {"0.5B", 0, true},
		{"9223372036854775807B", math.MaxInt64, true}, {"9223372036854775808B", 0, false},
		{"1XB", 0, false}, {"-1B", 0, false}, {"1e3B", 0, false}, {"NaN", 0, false},
		{strings.Repeat("9", 129) + "B", 0, false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if value, known := dockerSize(tc.raw); value != tc.want || known != tc.known {
				t.Fatalf("size = %d/%v, want %d/%v", value, known, tc.want, tc.known)
			}
		})
	}
}

func TestResourceDockerFieldsHaveIndependentAvailability(t *testing.T) {
	raw := resourceDockerRow("synthetic-node", map[string]string{"CPUPerc": "NaN%", "NetIO": "unknown / 2kB", "BlockIO": "1XB / 4KiB"})
	stat := dockerRows([]byte(raw), map[string]bool{"synthetic-node": true})["synthetic-node"]
	for key, want := range map[string]bool{"cpu": false, "mem": true, "netRx": false, "netTx": true, "blkRead": false, "blkWrite": true} {
		if stat.Known[key] != want {
			t.Fatalf("Docker field %s availability = %v, want %v", key, stat.Known[key], want)
		}
	}
	if stat.Mem != 1572864 || stat.NetTx != 2000 || stat.BlockWrite != 4096 {
		t.Fatal("valid Docker fields were lost alongside an invalid field")
	}
	if dockerRows([]byte(raw+"{"), map[string]bool{"synthetic-node": true}) != nil {
		t.Fatal("malformed Docker batch retained measurements")
	}
}

func TestResourceDirectoryAllocationCannotWrap(t *testing.T) {
	rows := directoryRows([]byte("/data 17 9007199254740992\n"), []string{"/data"})
	size := rows["/data"]
	if size.Bytes != 17 || !size.Known["bytes"] || size.Known["allocated"] {
		t.Fatal("allocated-byte overflow wrapped or discarded the independent byte observation")
	}
	var decoded DirSize
	if err := decodeMeasurementJSON(strings.NewReader(`{"bytes":0,"known":{"bytes":true,"allocated":true}}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Known["bytes"] || decoded.Known["allocated"] {
		t.Fatal("declared availability invented an absent directory field")
	}
}

func TestResourceEngineAggregateRequiresCompletePresentComponents(t *testing.T) {
	names := V1Names()
	rows := []map[string]any{
		resourceSample(1, map[string]any{names.EngineCandidates[0]: map[string]any{"cpu": 10, "mem": 1 << 20},
			names.EngineCandidates[1]: map[string]any{"cpu": 20, "mem": 2 << 20}}, nil),
		resourceSample(2, map[string]any{names.EngineCandidates[0]: map[string]any{"cpu": 100, "mem": 1 << 20, "known": map[string]bool{"cpu": false, "mem": true}},
			names.EngineCandidates[1]: map[string]any{"cpu": 40, "mem": 2 << 20}}, nil),
		resourceSample(3, map[string]any{names.EngineCandidates[0]: map[string]any{"cpu": 10, "mem": int64(math.MaxInt64)},
			names.EngineCandidates[1]: map[string]any{"cpu": 20, "mem": 1}}, nil),
	}
	w := resourceSummary(t, &names, rows).Windows[0]
	if w.EngineCPU != 30 || w.EngineMem != 3<<20 || w.Resources.EngineCPU != 2 || w.Resources.EngineMem != 2 {
		t.Fatal("partial or overflowing engine components entered a metric denominator")
	}
}

func TestResourceMeasuredZerosHaveUsableDenominators(t *testing.T) {
	names := V1Names()
	rows := []map[string]any{resourceSample(1, map[string]any{
		names.Node: map[string]any{"cpu": 0, "mem": 0}, names.EngineCandidates[0]: map[string]any{"cpu": 0, "mem": 0},
		names.Relayer: map[string]any{"cpu": 0},
	}, 0)}
	s := resourceSummary(t, &names, rows)
	cells := sampleAccountingCells(t, s, "00:00")
	for column, want := range map[int]string{7: "0", 10: "0.0", 11: "0.0", 12: "0.0", 13: "0", 14: "0"} {
		if cells[column] != want {
			t.Fatalf("measured zero resource column %d = %q", column, cells[column])
		}
	}
	if c := s.Windows[0].Resources; c.NodeCPU != 1 || c.EngineCPU != 1 || c.RelayerCPU != 1 || c.NodeMem != 1 || c.EngineMem != 1 || c.RelayerQueued != 1 {
		t.Fatal("measured zeros were omitted from resource denominators")
	}
}

func TestResourceMeanDoesNotOverflowForFiniteObservations(t *testing.T) {
	var mean float64
	var count int
	resourceMean(&mean, &count, math.MaxFloat64)
	resourceMean(&mean, &count, math.MaxFloat64)
	if mean != math.MaxFloat64 || count != 2 {
		t.Fatal("finite resource observations overflowed their mean")
	}
}
