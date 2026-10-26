package fleet

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func resourceFixture(t *testing.T, names *Names, samples []map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	writeSampleAccountingLines(t, filepath.Join(dir, "records.jsonl"), []map[string]any{
		{"t": sampleAccountingStart.UnixMilli(), "profile": "light", "kind": "read", "action": "synthetic-read", "ok": true},
		{"t": sampleAccountingStart.Add(40 * time.Minute).UnixMilli(), "profile": "light", "kind": "read", "action": "synthetic-read", "ok": true},
	})
	writeSampleAccountingLines(t, filepath.Join(dir, "samples.jsonl"), samples)
	if names != nil {
		writeReportJSON(t, filepath.Join(dir, "names.json"), names)
	}
	return dir
}

func resourceSample(seconds int, containers map[string]any, queued any) map[string]any {
	row := map[string]any{"t": sampleAccountingStart.Add(time.Duration(seconds) * time.Second).UnixMilli(), "containers": containers}
	if queued != nil {
		row["relayer"] = map[string]any{"queued": queued}
	}
	return row
}

func resourceSummary(t *testing.T, names *Names, samples []map[string]any) *UsersSummary {
	t.Helper()
	summary, err := SummarizeUsers(resourceFixture(t, names, samples))
	if err != nil {
		t.Fatal(err)
	}
	return summary
}

func TestResourcePerMetricMeansAndProfiles(t *testing.T) {
	for _, names := range []Names{V1Names(), DPNames(), ZVMDevnetNames()} {
		t.Run(names.Node, func(t *testing.T) {
			rows := []map[string]any{
				resourceSample(1, map[string]any{names.Node: map[string]any{"cpu": 20, "mem": 1 << 20},
					names.EngineCandidates[0]: map[string]any{"cpu": 40, "mem": 2 << 20}, names.Relayer: map[string]any{"cpu": 10}}, 0),
				resourceSample(2, map[string]any{}, nil),
			}
			cells := sampleAccountingCells(t, resourceSummary(t, &names, rows), "00:00")
			for column, want := range map[int]string{7: "0", 10: "20.0", 11: "40.0", 12: "10.0", 13: "1", 14: "2"} {
				if cells[column] != want {
					t.Errorf("resource column %d = %q, want %q; missing observations must not dilute the mean", column, cells[column], want)
				}
			}
		})
	}
}

func TestResourceMissingAndInvalidValuesRemainUnavailable(t *testing.T) {
	names := V1Names()
	for _, tc := range []struct {
		name string
		stat map[string]any
	}{
		{"missing", map[string]any{}},
		{"negative", map[string]any{"cpu": -1, "mem": -1}},
		{"explicitly-unavailable", map[string]any{"cpu": 20, "mem": 1 << 20, "known": map[string]bool{"cpu": false, "mem": false}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := resourceSample(1, map[string]any{names.Node: tc.stat, names.EngineCandidates[0]: tc.stat, names.Relayer: tc.stat}, -1)
			cells := sampleAccountingCells(t, resourceSummary(t, &names, []map[string]any{row}), "00:00")
			for _, column := range []int{7, 10, 11, 12, 13, 14} {
				if cells[column] != "n/a" {
					t.Errorf("unusable resource column %d = %q, want n/a", column, cells[column])
				}
			}
		})
	}
}

func TestResourceRoleSelectionExcludesUnrelatedContainers(t *testing.T) {
	names := V1Names()
	row := resourceSample(1, map[string]any{
		names.Node: map[string]any{"cpu": 20, "mem": 1 << 20}, names.Relayer: map[string]any{"cpu": 10},
		"unrelated-node": map[string]any{"cpu": 900, "mem": 100 << 20}, "unrelated-relayer": map[string]any{"cpu": 800},
	}, 0)
	cells := sampleAccountingCells(t, resourceSummary(t, &names, []map[string]any{row}), "00:00")
	if cells[10] != "20.0" || cells[12] != "10.0" || cells[13] != "1" {
		t.Fatal("unrelated containers contaminated the selected profile's resource measurements")
	}
}

func TestResourceCoverageUsesIndependentDenominators(t *testing.T) {
	names := V1Names()
	rows := []map[string]any{
		resourceSample(1, map[string]any{names.Node: map[string]any{"cpu": 20, "mem": 1 << 20},
			names.EngineCandidates[0]: map[string]any{"cpu": 40, "mem": 2 << 20}, names.Relayer: map[string]any{"cpu": 10}}, 0),
		resourceSample(2, map[string]any{names.Node: map[string]any{"mem": 2 << 20},
			names.EngineCandidates[0]: map[string]any{"mem": 3 << 20}, names.Relayer: map[string]any{"cpu": 20}}, -1),
	}
	summary := resourceSummary(t, &names, rows)
	raw, err := json.Marshal(summary.Windows[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	var coverage map[string]int
	if err := json.Unmarshal(fields["resourceObservations"], &coverage); err != nil {
		t.Fatal("resource summary omitted the actual per-metric denominators")
	}
	for key, want := range map[string]int{"samples": 2, "nodeCpu": 1, "engineCpu": 1, "relayerCpu": 2, "nodeMem": 2, "engineMem": 2, "relayerQueued": 1} {
		if coverage[key] != want {
			t.Errorf("resource denominator %s = %d, want %d", key, coverage[key], want)
		}
	}
}

func TestResourceUsersReportRejectsInvalidPresentProfile(t *testing.T) {
	for _, raw := range []string{"null", "{}", "{", `{"Node":"synthetic"}{}`} {
		t.Run(raw, func(t *testing.T) {
			dir := resourceFixture(t, nil, nil)
			if err := os.WriteFile(filepath.Join(dir, "names.json"), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := SummarizeUsers(dir); err == nil {
				t.Fatal("invalid present resource profile silently selected unrelated container names")
			}
		})
	}
}

func resourceDocker(t *testing.T, raw string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("synthetic Docker fixture uses a POSIX shell")
	}
	dir := t.TempDir()
	output, calls := filepath.Join(dir, "output"), filepath.Join(dir, "calls")
	if err := os.WriteFile(output, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FLEET_RESOURCE_TEST_CALLS\"\n/bin/cat \"$FLEET_RESOURCE_TEST_OUTPUT\"\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("FLEET_RESOURCE_TEST_OUTPUT", output)
	t.Setenv("FLEET_RESOURCE_TEST_CALLS", calls)
	return calls
}

func resourceDockerRow(name string, fields map[string]string) string {
	row := map[string]string{"Name": name, "CPUPerc": "25%", "MemUsage": "1.5MiB / 2MiB", "NetIO": "1kB / 2MB", "BlockIO": "3B / 4KiB"}
	for key, value := range fields {
		row[key] = value
	}
	raw, _ := json.Marshal(row)
	return string(raw) + "\n"
}

func TestResourceEmptySelectionDoesNotInvokeDocker(t *testing.T) {
	calls := resourceDocker(t, resourceDockerRow("unrelated", nil))
	e := &Env{Names: DPNames(), running: map[string]bool{}}
	stats := e.dockerStats(context.Background())
	if len(stats) != 0 {
		t.Error("empty selection observed unrelated Docker containers")
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Error("empty selection invoked Docker without container arguments")
	}
}

func TestResourceDockerSelectionRejectsForeignAndDuplicateRows(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign", true: "duplicate"}[duplicate], func(t *testing.T) {
			names := DPNames()
			raw := resourceDockerRow(names.Node, nil)
			if duplicate {
				raw += resourceDockerRow(names.Node, map[string]string{"CPUPerc": "50%"})
			} else {
				raw += resourceDockerRow("unrelated-node", nil)
			}
			resourceDocker(t, raw)
			e := &Env{Names: names, running: map[string]bool{names.Node: true}}
			stats := e.dockerStats(context.Background())
			if duplicate && len(stats) != 0 || !duplicate && (len(stats) != 1 || stats[names.Node].CPU != 25) {
				t.Fatal("Docker response escaped its selected container set or retained conflicting rows")
			}
		})
	}
}

func TestResourceDockerSizeKeepsFullSignedWidth(t *testing.T) {
	names := DPNames()
	resourceDocker(t, resourceDockerRow(names.Node, map[string]string{"MemUsage": "9223372036854775807B / 9223372036854775807B"}))
	e := &Env{Names: names, running: map[string]bool{names.Node: true}}
	if got := e.dockerStats(context.Background())[names.Node].Mem; got != int64(9223372036854775807) {
		t.Fatalf("Docker size rounded or wrapped: got %d, want 9223372036854775807", got)
	}
}

func TestResourceDirectoryCaptureRejectsForeignAndDuplicateRows(t *testing.T) {
	for _, raw := range []string{"/unselected 1 1\n", "/data 1 1\n/data 2 2\n"} {
		t.Run(strings.TrimSpace(raw), func(t *testing.T) {
			resourceDocker(t, raw)
			e := &Env{Names: Names{Node: "synthetic-node"}, running: map[string]bool{"synthetic-node": true}}
			if got := e.dirSizes(context.Background()); len(got) != 0 {
				t.Fatal("directory response escaped selected paths or retained conflicting rows")
			}
		})
	}
}

func TestResourcePhaseRequiresIndividualFieldAvailability(t *testing.T) {
	for _, tc := range []struct {
		name   string
		column int
		edit   func(map[string]any)
	}{
		{"missing-directory-bytes", 4, func(s map[string]any) {
			s["dirs"].(map[string]any)["zvm-l1-pillar:/root/.znn/nom"] = map[string]any{"allocated": 100}
		}},
		{"unknown-directory-bytes", 4, func(s map[string]any) {
			s["dirs"].(map[string]any)["zvm-l1-pillar:/root/.znn/nom"] = map[string]any{"bytes": 100, "known": map[string]bool{"bytes": false}}
		}},
		{"missing-write-counter", 13, func(s map[string]any) { s["containers"].(map[string]any)["zvm-node"] = map[string]any{"cpu": 1} }},
		{"unknown-write-counter", 13, func(s map[string]any) {
			s["containers"].(map[string]any)["zvm-node"] = map[string]any{"blkWrite": 100, "known": map[string]bool{"blkWrite": false}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := accountingSnapshot(), accountingSnapshot()
			tc.edit(before)
			if cell := accountingRow(t, before, after)[tc.column]; cell != "n/a" {
				t.Fatalf("individual unavailable resource field produced %q, want n/a", cell)
			}
		})
	}
}
