package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

func resourceFields(raw []byte) (map[string]json.RawMessage, map[string]bool, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, false, err
	}
	declared, present := fields["known"]
	var flags map[string]bool
	if present {
		if err := json.Unmarshal(declared, &flags); err != nil {
			return nil, nil, false, fmt.Errorf("resource known flags: %w", err)
		}
	}
	return fields, flags, present, nil
}

func resourceInteger(raw json.RawMessage) (int64, bool) {
	var value any
	if decodeMeasurementJSON(bytes.NewReader(raw), &value) != nil {
		return 0, false
	}
	n, ok := counterValue(value)
	if !ok || n > math.MaxInt64 {
		return 0, false
	}
	return int64(n), true
}

func resourceCPU(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil || !usableCPU(value) {
		return 0, false
	}
	return value + 0, true
}

func usableCPU(value float64) bool { return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0) }

func resourceKnown(flags map[string]bool, key string) bool { return flags == nil || flags[key] }

func (c *ContainerStat) UnmarshalJSON(raw []byte) error {
	fields, flags, declared, err := resourceFields(raw)
	if err != nil {
		return err
	}
	value := ContainerStat{Known: map[string]bool{}}
	value.CPU, value.Known["cpu"] = resourceCPU(fields["cpu"])
	for _, field := range []struct {
		key string
		out *int64
	}{
		{"mem", &value.Mem}, {"netRx", &value.NetRx}, {"netTx", &value.NetTx},
		{"blkRead", &value.BlockRead}, {"blkWrite", &value.BlockWrite},
	} {
		*field.out, value.Known[field.key] = resourceInteger(fields[field.key])
	}
	if declared {
		for key, valid := range value.Known {
			value.Known[key] = valid && flags[key]
		}
	}
	*c = value
	return nil
}

func (d *DirSize) UnmarshalJSON(raw []byte) error {
	fields, flags, declared, err := resourceFields(raw)
	if err != nil {
		return err
	}
	value := DirSize{Known: map[string]bool{}}
	value.Bytes, value.Known["bytes"] = resourceInteger(fields["bytes"])
	value.Allocated, value.Known["allocated"] = resourceInteger(fields["allocated"])
	if declared {
		for key, valid := range value.Known {
			value.Known[key] = valid && flags[key]
		}
	}
	*d = value
	return nil
}

func dockerCPU(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) > 128 || !strings.HasSuffix(raw, "%") {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(raw, "%")), 64)
	if err != nil || !usableCPU(value) {
		return 0, false
	}
	return value + 0, true
}

// dockerSize converts Docker's rounded human display with exact decimal
// scaling and truncation. It cannot recover bytes lost by that display.
func dockerSize(raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > 128 {
		return 0, false
	}
	i := strings.IndexFunc(raw, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	number, unit := raw, ""
	if i >= 0 {
		number, unit = raw[:i], strings.TrimSpace(raw[i:])
	}
	if number == "" || strings.Count(number, ".") > 1 || number == "." {
		return 0, false
	}
	multiplier, ok := map[string]int64{"": 1, "B": 1, "kB": 1000, "KB": 1000, "MB": 1000000,
		"GB": 1000000000, "TB": 1000000000000, "KiB": 1 << 10, "MiB": 1 << 20,
		"GiB": 1 << 30, "TiB": 1 << 40}[unit]
	if !ok {
		return 0, false
	}
	ratio, ok := new(big.Rat).SetString(number)
	if !ok || ratio.Sign() < 0 {
		return 0, false
	}
	ratio.Mul(ratio, new(big.Rat).SetInt64(multiplier))
	value := new(big.Int).Quo(ratio.Num(), ratio.Denom())
	if !value.IsInt64() {
		return 0, false
	}
	return value.Int64(), true
}

func dockerSizePair(raw string) (int64, bool, int64, bool) {
	a, b, found := strings.Cut(raw, "/")
	if !found || strings.Contains(b, "/") {
		return 0, false, 0, false
	}
	left, leftKnown := dockerSize(a)
	right, rightKnown := dockerSize(b)
	return left, leftKnown, right, rightKnown
}

func dockerRows(raw []byte, allowed map[string]bool) map[string]ContainerStat {
	result := map[string]ContainerStat{}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row struct{ Name, CPUPerc, MemUsage, NetIO, BlockIO string }
		if json.Unmarshal([]byte(line), &row) != nil || row.Name == "" {
			return nil
		}
		if !allowed[row.Name] {
			continue
		}
		if seen[row.Name] {
			delete(result, row.Name)
			continue
		}
		seen[row.Name] = true
		cpu, cpuKnown := dockerCPU(row.CPUPerc)
		mem, memKnown, _, _ := dockerSizePair(row.MemUsage)
		rx, rxKnown, tx, txKnown := dockerSizePair(row.NetIO)
		read, readKnown, write, writeKnown := dockerSizePair(row.BlockIO)
		result[row.Name] = ContainerStat{CPU: cpu, Mem: mem, NetRx: rx, NetTx: tx, BlockRead: read, BlockWrite: write,
			Known: map[string]bool{"cpu": cpuKnown, "mem": memKnown, "netRx": rxKnown, "netTx": txKnown,
				"blkRead": readKnown, "blkWrite": writeKnown}}
	}
	return result
}

func directoryRows(raw []byte, paths []string) map[string]DirSize {
	result := map[string]DirSize{}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !contains(paths, fields[0]) {
			continue
		}
		path := fields[0]
		if seen[path] {
			delete(result, path)
			continue
		}
		seen[path] = true
		if len(fields) != 3 {
			continue
		}
		b, bKnown := counterValue(fields[1])
		k, kKnown := counterValue(fields[2])
		bKnown, kKnown = bKnown && b <= math.MaxInt64, kKnown && k <= math.MaxInt64/1024
		size := DirSize{Known: map[string]bool{"bytes": bKnown, "allocated": kKnown}}
		if bKnown {
			size.Bytes = int64(b)
		}
		if kKnown {
			size.Allocated = int64(k) * 1024
		}
		result[path] = size
	}
	return result
}

// ResourceObservations gives each metric its own usable sample denominator.
type ResourceObservations struct {
	Samples       int `json:"samples"`
	NodeCPU       int `json:"nodeCpu"`
	EngineCPU     int `json:"engineCpu"`
	RelayerCPU    int `json:"relayerCpu"`
	NodeMem       int `json:"nodeMem"`
	EngineMem     int `json:"engineMem"`
	RelayerQueued int `json:"relayerQueued"`
}

func resourceMean(mean *float64, count *int, value float64) {
	*count += 1
	*mean += (value - *mean) / float64(*count)
}

func cpuObserved(c ContainerStat) bool { return resourceKnown(c.Known, "cpu") && usableCPU(c.CPU) }
func memObserved(c ContainerStat) bool { return resourceKnown(c.Known, "mem") && c.Mem >= 0 }

func (w *Window) observeResources(sample Sample, names Names) {
	cov := &w.Resources
	cov.Samples++
	if q, valid := counterValue(sample.Relayer["queued"]); valid {
		cov.RelayerQueued++
		w.Queued = max(w.Queued, q)
	}
	if c, present := sample.Containers[names.Node]; present && names.Node != "" {
		if cpuObserved(c) {
			resourceMean(&w.NodeCPU, &cov.NodeCPU, c.CPU)
		}
		if memObserved(c) {
			cov.NodeMem++
			w.NodeMem = max(w.NodeMem, c.Mem)
		}
	}
	if c, present := sample.Containers[names.Relayer]; present && names.Relayer != "" && cpuObserved(c) {
		resourceMean(&w.RelayerCPU, &cov.RelayerCPU, c.CPU)
	}
	seen := map[string]bool{}
	saw, cpuKnown, memKnown := false, true, true
	var cpu float64
	var mem int64
	for _, name := range names.EngineCandidates {
		c, present := sample.Containers[name]
		if !present || name == "" || seen[name] {
			continue
		}
		seen[name], saw = true, true
		if !cpuObserved(c) || math.IsInf(cpu+c.CPU, 0) {
			cpuKnown = false
		} else {
			cpu += c.CPU
		}
		if !memObserved(c) || c.Mem > math.MaxInt64-mem {
			memKnown = false
		} else {
			mem += c.Mem
		}
	}
	if saw && cpuKnown {
		resourceMean(&w.EngineCPU, &cov.EngineCPU, cpu)
	}
	if saw && memKnown {
		cov.EngineMem++
		w.EngineMem = max(w.EngineMem, mem)
	}
}

func resourcePercent(value float64, count int) string {
	if count == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f", value)
}

func resourceMemory(value int64, count int) string {
	if count == 0 {
		return "n/a"
	}
	return mib(value)
}
