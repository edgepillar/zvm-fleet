package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

// engineDir is one client's directory for a kind of engine storage, and which
// of the two measures to read for it.
type engineDir struct {
	Key string
	// Allocated reads what the filesystem actually gave the directory rather
	// than the apparent length of its files.
	Allocated bool
}

func runningContainers(ctx context.Context) (map[string]bool, error) {
	out, err := measurementCommand(ctx, "ps", "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	running := map[string]bool{}
	for _, name := range strings.Fields(string(out)) {
		running[name] = true
	}
	return running, nil
}

func (e *Env) installProbe(ctx context.Context) error {
	if e.Probe == "" {
		return fmt.Errorf("no probe binary: build one with GOOS=linux GOARCH=amd64 CGO_ENABLED=0")
	}
	for _, c := range []string{e.Names.Node, e.Names.NodeB} {
		if c == "" || !e.running[c] {
			continue
		}
		if _, err := measurementCommand(ctx, "cp", e.Probe, c+":/tmp/zvm-probe"); err != nil {
			return fmt.Errorf("docker cp to %s: %w", c, err)
		}
		if _, err := measurementCommand(ctx, "exec", c, "chmod", "+x", "/tmp/zvm-probe"); err != nil {
			return fmt.Errorf("chmod in %s: %w", c, err)
		}
	}
	return nil
}

type ContainerStat struct {
	Known      map[string]bool `json:"known,omitempty"`
	CPU        float64         `json:"cpu"` // percent of one core
	Mem        int64           `json:"mem"`
	NetRx      int64           `json:"netRx"`
	NetTx      int64           `json:"netTx"`
	BlockRead  int64           `json:"blkRead"`
	BlockWrite int64           `json:"blkWrite"`
}

func (e *Env) dockerStats(ctx context.Context) map[string]ContainerStat {
	stats, _ := e.dockerStatsResult(ctx)
	return stats
}

func (e *Env) dockerStatsResult(ctx context.Context) (map[string]ContainerStat, error) {
	var names []string
	allowed := map[string]bool{}
	for _, c := range e.Names.all() {
		if e.running[c] && !allowed[c] {
			names = append(names, c)
			allowed[c] = true
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	args := append([]string{"stats", "--no-stream", "--format", "{{json .}}"}, names...)
	out, err := measurementCommand(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("command unavailable: %w", err)
	}
	stats := dockerRows(out, allowed)
	if stats == nil {
		return nil, fmt.Errorf("malformed output")
	}
	if len(stats) != len(allowed) {
		return stats, fmt.Errorf("incomplete observations")
	}
	for _, stat := range stats {
		for _, known := range stat.Known {
			if !known {
				return stats, fmt.Errorf("invalid fields")
			}
		}
	}
	return stats, nil
}

type DirSize struct {
	Known     map[string]bool `json:"known,omitempty"`
	Bytes     int64           `json:"bytes"`
	Allocated int64           `json:"allocated"`
}

func (e *Env) dirSizes(ctx context.Context, only ...string) map[string]DirSize {
	out, _ := e.dirSizesResult(ctx, only...)
	return out
}

func (e *Env) dirSizesResult(ctx context.Context, only ...string) (map[string]DirSize, error) {
	out := map[string]DirSize{}
	var failures []error
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, d := range e.Names.measuredDirs() {
		if !e.running[d.container] {
			continue
		}
		if len(only) > 0 && !contains(only, d.container) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var script strings.Builder
			for _, p := range d.paths {
				fmt.Fprintf(&script, "echo %s $(du -s -b %s 2>/dev/null | cut -f1) $(du -s -k %s 2>/dev/null | cut -f1);", p, p, p)
			}
			raw, err := measurementCommand(ctx, "exec", d.container, "sh", "-c", script.String())
			if err != nil {
				mu.Lock()
				failures = append(failures, fmt.Errorf("directory measurements for %s: %w", d.container, err))
				mu.Unlock()
				return
			}
			for path, size := range directoryRows(raw, d.paths) {
				mu.Lock()
				out[d.container+":"+path] = size
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out, errors.Join(failures...)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (e *Env) probe(ctx context.Context, container string, media bool) (*bench.StorageReport, error) {
	report, _, _, err := e.probeObserved(ctx, container, media)
	return report, err
}

func (e *Env) probeObserved(ctx context.Context, container string, media bool) (*bench.StorageReport, bool, bool, error) {
	if !e.running[container] {
		return nil, false, false, nil
	}
	args := []string{"exec", container, "/tmp/zvm-probe", "bench", "storage", "-db", "/data/zvm.db"}
	if media {
		args = append(args, "-media", "/data/media")
	}
	raw, err := measurementCommand(ctx, args...)
	if err != nil {
		return nil, false, false, fmt.Errorf("storage probe: %w", err)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return nil, false, false, fmt.Errorf("storage probe must return an object")
	}
	rep := new(bench.StorageReport)
	if err := json.Unmarshal(raw, rep); err != nil {
		return nil, false, false, err
	}
	file, pages, err := storageAvailability(raw, rep)
	if err != nil {
		return nil, false, false, fmt.Errorf("storage probe fields: %w", err)
	}
	return rep, file, pages, nil
}

func (e *Env) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.API+path, nil)
	if err != nil {
		return err
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return decodeMeasurementJSON(resp.Body, out)
}

func pick(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

// Snapshot is everything a phase's cost is computed from.
type Snapshot struct {
	IndexFileBytesKnown *bool                    `json:"indexFileBytesKnown,omitempty"`
	IndexPagesKnown     *bool                    `json:"indexPagesKnown,omitempty"`
	FollowerPagesKnown  *bool                    `json:"followerPagesKnown,omitempty"`
	At                  time.Time                `json:"at"`
	TookMS              int64                    `json:"tookMs"`
	Containers          map[string]ContainerStat `json:"containers"`
	Dirs                map[string]DirSize       `json:"dirs"`
	Index               *bench.StorageReport     `json:"index,omitempty"`
	FollowerDB          *bench.StorageReport     `json:"followerDb,omitempty"`
	Ops                 map[string]any           `json:"ops,omitempty"`
	Services            map[string]any           `json:"services,omitempty"`
	L2Head              uint64                   `json:"l2Head"`
	L2HeadKnown         *bool                    `json:"l2HeadKnown,omitempty"`
	FollowerHead        uint64                   `json:"followerHead"`
	VaultHeight         uint64                   `json:"vaultHeight"`
	Errors              []string                 `json:"errors,omitempty"`
}

func (e *Env) snapshot(ctx context.Context) *Snapshot {
	start := time.Now()
	s := &Snapshot{At: start.UTC()}
	var mu sync.Mutex
	fail := func(what string, err error) {
		mu.Lock()
		s.Errors = append(s.Errors, what+": "+measurementReportReason(err))
		mu.Unlock()
	}
	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		var err error
		s.Containers, err = e.dockerStatsResult(ctx)
		if err != nil {
			fail("containers", err)
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		s.Dirs, err = e.dirSizesResult(ctx)
		if err != nil {
			fail("directories", err)
		}
	}()
	go func() {
		defer wg.Done()
		r, file, pages, err := e.probeObserved(ctx, e.Names.Node, true)
		if err != nil {
			fail("index", err)
		}
		s.Index = r
		s.IndexFileBytesKnown, s.IndexPagesKnown = &file, &pages
	}()
	go func() {
		defer wg.Done()
		r, _, pages, err := e.probeObserved(ctx, e.Names.NodeB, false)
		if err != nil {
			fail("follower db", err)
		}
		s.FollowerDB = r
		s.FollowerPagesKnown = &pages
	}()
	go func() {
		defer wg.Done()
		var ops map[string]any
		if err := e.getJSON(ctx, "/api/ops", &ops); err != nil {
			fail("ops", err)
		} else {
			s.Ops = pick(ops, "derivation", "engine", "l1")
		}
		var info map[string]any
		if err := e.getJSON(ctx, "/api/info", &info); err != nil {
			fail("info", err)
		} else {
			s.Services = pick(info, "relayer", "bridge", "faucet", "l1", "l2", "derivation")
			if r, ok := s.Services["relayer"].(map[string]any); ok {
				delete(r, "accounts")
			}
		}
	}()
	wg.Wait()
	var headErr error
	s.L2Head, headErr = e.Eth.BlockNumber(ctx)
	headKnown := headErr == nil
	s.L2HeadKnown = &headKnown
	if headErr != nil {
		fail("l2 head", headErr)
	}
	if e.Follower != nil {
		s.FollowerHead, _ = e.Follower.BlockNumber(ctx)
	}
	if fb, err := e.L1.FrontierAccountBlock(ctx, e.Rollup.Vault); err == nil && fb != nil {
		s.VaultHeight = fb.Height
	}
	s.TookMS = time.Since(start).Milliseconds()
	return s
}

// Sample is the five-second view of a phase.
type Sample struct {
	RunID      string                   `json:"runId,omitempty"`
	T          int64                    `json:"t"`
	Known      *SampleKnown             `json:"known,omitempty"`
	Containers map[string]ContainerStat `json:"containers,omitempty"`
	L2         uint64                   `json:"l2"`
	Indexed    uint64                   `json:"indexed"`
	Follower   uint64                   `json:"follower,omitempty"`
	L1         uint64                   `json:"l1"`
	Derived    uint64                   `json:"derived"`
	Relayer    map[string]any           `json:"relayer,omitempty"`
	Bridge     map[string]any           `json:"bridge,omitempty"`
	Faucet     map[string]any           `json:"faucet,omitempty"`
	Errors     []string                 `json:"errors,omitempty"`
}

func (e *Env) sampleLoop(ctx context.Context, rec *PhaseRecord) {
	every := e.SampleEvery
	if every <= 0 {
		every = 5 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		s := e.sampleOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		rec.mu.Lock()
		rec.Samples = append(rec.Samples, s)
		rec.mu.Unlock()
		if e.onSample != nil {
			if err := e.onSample(s); err != nil {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func num(v any) uint64 {
	n, ok := counterValue(v)
	if !ok {
		return 0
	}
	return n
}

func (e *Env) sampleOnce(ctx context.Context) Sample {
	s := Sample{T: time.Now().UnixMilli(), RunID: e.userRunToken(), Known: new(SampleKnown)}
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		var err error
		s.Containers, err = e.dockerStatsResult(ctx)
		if err != nil {
			s.Errors = append(s.Errors, "docker stats: "+measurementReportReason(err))
		}
	}()
	go func() {
		defer wg.Done()
		var info map[string]any
		if e.getJSON(ctx, "/api/info", &info) != nil {
			return
		}
		s.captureInfo(info)
	}()
	go func() {
		defer wg.Done()
		var rows []map[string]any
		if e.getJSON(ctx, "/api/blocks?limit=1", &rows) == nil && len(rows) > 0 {
			s.Indexed, s.Known.Indexed = counterValue(rows[0]["number"])
		}
	}()
	go func() {
		defer wg.Done()
		if e.Follower != nil {
			var err error
			s.Follower, err = e.Follower.BlockNumber(ctx)
			s.Known.Follower = err == nil
		}
	}()
	wg.Wait()
	return s
}
