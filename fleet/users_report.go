package fleet

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// UsersSummary is a user simulation reduced to what the report shows. It is
// rebuilt from records.jsonl and samples.jsonl, so a run that was interrupted
// still reports the part it ran.
type UsersSummary struct {
	Label    string            `json:"label"`
	Mix      []ProfileCount    `json:"mix"`
	Start    time.Time         `json:"start"`
	End      time.Time         `json:"end"`
	Totals   UsersTotals       `json:"totals"`
	Profiles []*Agg            `json:"profiles"`
	Actions  []*Agg            `json:"actions"`
	Windows  []*Window         `json:"windows"`
	Errors   []ErrorCount      `json:"errors"`
	Inputs   UserInputEvidence `json:"inputs"`
	Phase    *PhaseRecord      `json:"-"`
	context  userReportContext
}

type UsersTotals struct {
	Hours         float64 `json:"hours"`
	Txs           int     `json:"txs"`
	Confirmed     int     `json:"confirmed"`
	Reverted      int     `json:"reverted"`
	Failed        int     `json:"failed"` // refused, or no receipt
	Skipped       int     `json:"skipped"`
	Reads         int     `json:"reads"`
	ReadErrors    int     `json:"readErrors"`
	Orders        int     `json:"orders"`
	Payouts       int     `json:"payouts"`
	PayoutsPaid   int     `json:"payoutsPaid"`
	TxPerSec      float64 `json:"txPerSec"`
	PeakTxPerSec  float64 `json:"peakTxPerSec"` // busiest window
	RateKnown     bool    `json:"rateKnown"`
	PeakRateKnown bool    `json:"peakRateKnown"`
	RateBasis     string  `json:"rateBasis"`
	Confirm       Dist    `json:"confirmMs"`
	Include       Dist    `json:"includeMs"`
	Read          Dist    `json:"readMs"`
	GasTotal      uint64  `json:"gasTotal"`
	Blocks        int     `json:"blocks"` // distinct L2 blocks holding a user transaction
}

// Agg is one row of the by-profile or by-action tables.
type Agg struct {
	Name         string  `json:"name"`
	Kind         string  `json:"kind"`
	Users        int     `json:"users,omitempty"`
	N            int     `json:"n"`
	OK           int     `json:"ok"`
	Reverted     int     `json:"reverted"`
	Failed       int     `json:"failed"`
	Skipped      int     `json:"skipped"`
	Reads        int     `json:"reads"`
	GasMean      float64 `json:"gasMean"`
	PerHour      float64 `json:"perUserHour,omitempty"` // transactions per user-hour
	PerHourKnown bool    `json:"perUserHourKnown"`
	Confirm      Dist    `json:"confirmMs"`
	Latency      Dist    `json:"latencyMs"`
	confirm      []float64
	latency      []float64
	gas          uint64
	gasN         int
}

// Window is one slice of the run in time.
type Window struct {
	Start         time.Time `json:"start"`
	Txs           int       `json:"txs"`
	OK            int       `json:"ok"`
	Failed        int       `json:"failed"`
	TxPerSec      float64   `json:"txPerSec"`
	RateKnown     bool      `json:"rateKnown"`
	Confirm       Dist      `json:"confirmMs"`
	Read          Dist      `json:"readMs"`
	Queued        uint64    `json:"relayerQueuedMax"`
	NodeCPU       float64   `json:"nodeCpu"`
	EngineCPU     float64   `json:"engineCpu"`
	RelayerCPU    float64   `json:"relayerCpu"`
	NodeMem       int64     `json:"nodeMem"`
	EngineMem     int64     `json:"engineMem"`
	L2Blocks      uint64    `json:"l2Blocks"`
	L1Moms        uint64    `json:"l1Momentums"`
	L2BlocksKnown bool      `json:"l2BlocksKnown"`
	L1MomsKnown   bool      `json:"l1MomentumsKnown"`
	confirm       []float64
	read          []float64
	Resources     ResourceObservations `json:"resourceObservations"`
	l2, l1        sampleCounterWindow
}

type ErrorCount struct {
	Action string `json:"action"`
	Error  string `json:"error"`
	N      int    `json:"n"`
}

// WindowSize is the report's time slice.
const WindowSize = 30 * time.Minute

func SummarizeUsers(dir string) (*UsersSummary, error) {
	s := &UsersSummary{}
	if err := loadUserContext(dir, s); err != nil {
		return nil, err
	}
	names := s.context.names
	var err error

	var recs []Record
	s.Inputs.Records, err = readUserJSONL(filepath.Join(dir, "records.jsonl"), false, func(raw []byte) error {
		record, err := userRecord(raw)
		if err != nil {
			return err
		}
		id, err := rowRunID(raw)
		if err != nil {
			return err
		}
		if err := s.observeRunID("records.jsonl", id); err != nil {
			return err
		}
		if err := s.checkRecordContext(record); err != nil {
			return err
		}
		recs = append(recs, *record)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("%s: no records", dir)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].T < recs[j].T })
	s.Start = time.UnixMilli(recs[0].T)
	if s.context.start != nil {
		s.Start = *s.context.start
	}
	s.End = time.UnixMilli(recs[len(recs)-1].T)
	s.Inputs.Temporal = UserTimeEvidence{Basis: "retained-record-start-span", Start: s.Start, End: s.End, LastRetainedRecordMS: recs[len(recs)-1].T}
	if s.context.end != nil {
		s.End = *s.context.end
		s.Inputs.Temporal.Basis = "declared-simulation"
	} else if s.context.start != nil {
		s.Inputs.Temporal.Basis = "declared-start-retained-prefix"
	}
	s.Inputs.Temporal.End = s.End
	anchor := s.Start.Truncate(time.Minute)
	if s.Start.After(s.End) || s.End.Sub(anchor)/WindowSize >= userReportWindowLimit {
		return nil, fmt.Errorf("user report time span exceeds the window bound or conflicts with its context")
	}
	s.Inputs.Records.UsedRows = s.Inputs.Records.Rows

	users := map[string]map[int]bool{}
	profiles := map[string]*Agg{}
	actions := map[string]*Agg{}
	errs := map[[2]string]int{}
	var windows []*Window
	window := func(t time.Time) *Window {
		i := int(t.Sub(anchor) / WindowSize)
		if s.context.end != nil && t.Equal(s.End) && i > 0 && t.Equal(anchor.Add(time.Duration(i)*WindowSize)) {
			i-- // the final endpoint belongs to the last active window
		}
		if i < 0 {
			i = 0
		}
		for len(windows) <= i {
			windows = append(windows, &Window{Start: anchor.Add(time.Duration(len(windows)) * WindowSize)})
		}
		return windows[i]
	}
	var confirm, include, read []float64
	blocks := map[uint64]bool{}
	t := &s.Totals
	for _, r := range recs {
		if users[r.Profile] == nil {
			users[r.Profile] = map[int]bool{}
		}
		users[r.Profile][r.User] = true
		pa := profiles[r.Profile]
		if pa == nil {
			pa = &Agg{Name: r.Profile}
			profiles[r.Profile] = pa
		}
		aa := actions[r.Action]
		if aa == nil {
			aa = &Agg{Name: r.Action, Kind: r.Kind}
			actions[r.Action] = aa
		}
		var w *Window
		if r.Kind == "tx" || r.Kind == "read" {
			if time.UnixMilli(r.T).Sub(anchor)/WindowSize >= userReportWindowLimit {
				return nil, fmt.Errorf("user record exceeds the report window bound")
			}
			w = window(time.UnixMilli(r.T))
		}
		for _, a := range []*Agg{pa, aa} {
			switch {
			case r.Skipped != "":
				a.Skipped++
			case r.Kind == "read":
				a.Reads++
				if r.OK && a.Kind == "read" {
					a.OK++
				}
				a.latency = append(a.latency, float64(r.LatencyMS))
			default:
				a.N++
				switch {
				case r.OK:
					a.OK++
				case r.Reverted:
					a.Reverted++
				default:
					a.Failed++
				}
				if r.ConfirmMS > 0 {
					a.confirm = append(a.confirm, float64(r.ConfirmMS))
				}
				if r.Kind == "order" && r.OK {
					a.latency = append(a.latency, float64(r.LatencyMS))
				}
				if r.GasUsed > 0 {
					a.gas += r.GasUsed
					a.gasN++
				}
			}
		}
		if r.Error != "" && r.Skipped == "" {
			errs[[2]string{r.Action, errorKind(r.Error)}]++
		}
		switch {
		case r.Skipped != "":
			t.Skipped++
		case r.Kind == "read":
			t.Reads++
			if !r.OK {
				t.ReadErrors++
			}
			read = append(read, float64(r.LatencyMS))
			w.read = append(w.read, float64(r.LatencyMS))
		case r.Kind == "order":
			t.Orders++
		case r.Kind == "payout":
			t.Payouts++
			if r.OK {
				t.PayoutsPaid++
			}
		case r.Kind == "tx":
			t.Txs++
			w.Txs++
			switch {
			case r.OK:
				t.Confirmed++
				w.OK++
			case r.Reverted:
				t.Reverted++
			default:
				t.Failed++
				w.Failed++
			}
			if r.Block != 0 {
				blocks[r.Block] = true
				t.GasTotal += r.GasUsed
			}
			if r.ConfirmMS > 0 {
				confirm = append(confirm, float64(r.ConfirmMS))
				w.confirm = append(w.confirm, float64(r.ConfirmMS))
			}
			if r.IncludeMS > 0 {
				include = append(include, float64(r.IncludeMS))
			}
		}
	}
	simEnd := s.End
	t.Hours = simEnd.Sub(s.Start).Hours()
	t.RateKnown, t.RateBasis = simEnd.After(s.Start), s.Inputs.Temporal.Basis
	if t.Hours > 0 {
		t.TxPerSec = float64(t.Txs) / (t.Hours * 3600)
	}
	t.Confirm, t.Include, t.Read, t.Blocks = dist(confirm), dist(include), dist(read), len(blocks)

	// Samples: the resource and chain view, per window.
	var usedSamples int64
	s.Inputs.Samples, err = readUserJSONL(filepath.Join(dir, "samples.jsonl"), true, func(raw []byte) error {
		sm, err := userSample(raw)
		if err != nil {
			return err
		}
		id, err := rowRunID(raw)
		if err != nil {
			return err
		}
		if err := s.observeRunID("samples.jsonl", id); err != nil {
			return err
		}
		at := time.UnixMilli(sm.T)
		limit := simEnd.Add(WindowSize)
		if s.context.end != nil {
			limit = simEnd
		}
		if sm.T < s.Start.UnixMilli() || sm.T > limit.UnixMilli() {
			return nil
		}
		if at.Sub(anchor)/WindowSize >= userReportWindowLimit {
			return fmt.Errorf("user sample exceeds the report window bound")
		}
		usedSamples++
		w := window(at)
		w.observeResources(*sm, names)
		l2Known, l1Known := sm.L2 > 0, sm.L1 > 0
		if sm.Known != nil {
			l2Known, l1Known = sm.Known.L2, sm.Known.L1
		}
		w.l2.observe(sm.T, sm.L2, l2Known)
		w.l1.observe(sm.T, sm.L1, l1Known)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.Inputs.Samples.UsedRows = usedSamples
	c := s.context
	s.Inputs.Cohort.ContextComplete = c.marker && c.users && c.namesFile && c.phase && c.phaseUsers
	s.Inputs.Partial = s.Inputs.Records.State != "terminated" || s.Inputs.Samples.State != "terminated" || s.Inputs.Samples.Rows == 0 ||
		s.Inputs.Cohort.RunID != "" && !s.Inputs.Cohort.ContextComplete || c.start != nil && c.end == nil
	for _, w := range windows {
		lo, hi := w.Start, w.Start.Add(WindowSize)
		if lo.Before(s.Start) {
			lo = s.Start
		}
		if hi.After(simEnd) {
			hi = simEnd
		}
		w.RateKnown = t.RateKnown && hi.After(lo)
		if w.RateKnown {
			w.TxPerSec = float64(w.Txs) / hi.Sub(lo).Seconds()
			t.PeakTxPerSec = max(t.PeakTxPerSec, w.TxPerSec)
			t.PeakRateKnown = true
		}
		w.Confirm, w.Read = dist(w.confirm), dist(w.read)
		w.L2Blocks, w.L2BlocksKnown = w.l2.result()
		w.L1Moms, w.L1MomsKnown = w.l1.result()
	}
	s.Windows = windows

	count := map[string]int{}
	for _, pc := range s.Mix {
		count[pc.Profile.Name] = pc.Count
	}
	finish := func(a *Agg) {
		a.Confirm, a.Latency = dist(a.confirm), dist(a.latency)
		if a.gasN > 0 {
			a.GasMean = float64(a.gas) / float64(a.gasN)
		}
	}
	for _, p := range Profiles {
		a := profiles[p.Name]
		if a == nil {
			continue
		}
		a.Users = count[p.Name]
		if a.Users == 0 {
			a.Users = len(users[p.Name])
		}
		finish(a)
		if t.Hours > 0 && a.Users > 0 {
			a.PerHour = float64(a.N) / (float64(a.Users) * t.Hours)
			a.PerHourKnown = true
		}
		s.Profiles = append(s.Profiles, a)
	}
	for _, a := range actions {
		finish(a)
		s.Actions = append(s.Actions, a)
	}
	sort.Slice(s.Actions, func(i, j int) bool {
		if s.Actions[i].Kind != s.Actions[j].Kind {
			return kindOrder(s.Actions[i].Kind) < kindOrder(s.Actions[j].Kind)
		}
		return s.Actions[i].Name < s.Actions[j].Name
	})
	for k, n := range errs {
		s.Errors = append(s.Errors, ErrorCount{Action: k[0], Error: k[1], N: n})
	}
	sort.Slice(s.Errors, func(i, j int) bool { return s.Errors[i].N > s.Errors[j].N })
	return s, nil
}

func kindOrder(k string) int {
	return strings.Index("tx order payout read", k)
}

func secs(ms float64) string {
	if ms == 0 {
		return ""
	}
	return fmt.Sprintf("%.1f", ms/1000)
}

func pct(n, of int) string {
	if of == 0 {
		return ""
	}
	return fmt.Sprintf("%.2f %%", 100*float64(n)/float64(of))
}

func mib(b int64) string { return fmt.Sprintf("%.0f", float64(b)/(1<<20)) }

// UsersMarkdown renders a summary as the tables the benchmark docs carry.
func UsersMarkdown(s *UsersSummary) string {
	var b strings.Builder
	t := s.Totals
	users := 0
	for _, pc := range s.Mix {
		users += pc.Count
	}
	label := s.Label
	if label == "" {
		label = "users"
	}
	fmt.Fprintf(&b, "# ZVM user simulation — %s\n\n", label)
	fmt.Fprintf(&b, "Generated by `zvm-fleet users-report`. %d simulated users, %s → %s UTC (%.2f h).\n\n",
		users, s.Start.UTC().Format("2006-01-02 15:04"), s.End.UTC().Format("2006-01-02 15:04"), t.Hours)
	renderUserInputs(&b, s.Inputs)

	b.WriteString("## Population\n\n| Profile | Users | Lane | Mean think | In flight | Mix |\n|---|---:|---|---:|---:|---|\n")
	for _, pc := range s.Mix {
		p := pc.Profile
		var mix []string
		for _, k := range sortedKeys(p.Mix) {
			mix = append(mix, fmt.Sprintf("%s %g", k, p.Mix[k]))
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %d | %s |\n", p.Name, pc.Count, p.Lane, p.Think, p.Pipeline, strings.Join(mix, ", "))
	}

	b.WriteString("\n## Totals\n\n| | |\n|---|---:|\n")
	row := func(k, v string) { fmt.Fprintf(&b, "| %s | %s |\n", k, v) }
	row("Transactions submitted", n64(t.Txs))
	row("Confirmed", fmt.Sprintf("%s (%s)", n64(t.Confirmed), pct(t.Confirmed, t.Txs)))
	row("Reverted", fmt.Sprintf("%s (%s)", n64(t.Reverted), pct(t.Reverted, t.Txs)))
	row("Failed (refused or no receipt)", fmt.Sprintf("%s (%s)", n64(t.Failed), pct(t.Failed, t.Txs)))
	row("Skipped (nothing to do)", n64(t.Skipped))
	row("Mean offered tx/s", userRate(t.TxPerSec, t.RateKnown, 2))
	row(fmt.Sprintf("Busiest %s tx/s", WindowSize), userRate(t.PeakTxPerSec, t.PeakRateKnown, 2))
	row("Confirmation p50 / p90 / p99 / max, s", fmt.Sprintf("%s / %s / %s / %s", secs(t.Confirm.P50), secs(t.Confirm.P90), secs(t.Confirm.P99), secs(t.Confirm.Max)))
	row("Submit → block timestamp p50 / p90, s", fmt.Sprintf("%s / %s", secs(t.Include.P50), secs(t.Include.P90)))
	row("L2 blocks carrying a user tx", n64(t.Blocks))
	row("Gas used", n64(t.GasTotal))
	row("Signed orders posted", n64(t.Orders))
	row("Withdrawals paid on NoM", fmt.Sprintf("%d of %d", t.PayoutsPaid, t.Payouts))
	row("Page loads", fmt.Sprintf("%s (%d errors)", n64(t.Reads), t.ReadErrors))
	row("Page load p50 / p90 / p99, ms", fmt.Sprintf("%.0f / %.0f / %.0f", t.Read.P50, t.Read.P90, t.Read.P99))

	b.WriteString("\n## By profile\n\n| Profile | Users | Txs | Tx/user-hour | OK | Reverted | Failed | Skipped | Confirm p50 s | p90 s | p99 s | Page loads | Load p50 ms |\n|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, a := range s.Profiles {
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %d | %d | %d | %s | %s | %s | %s | %.0f |\n", a.Name, a.Users, n64(a.N), userRate(a.PerHour, a.PerHourKnown, 1),
			n64(a.OK), a.Reverted, a.Failed, a.Skipped, secs(a.Confirm.P50), secs(a.Confirm.P90), secs(a.Confirm.P99), n64(a.Reads), a.Latency.P50)
	}

	b.WriteString("\n## By action\n\nConfirmation is submit → receipt seen, what a wallet shows as pending. A payout is a withdrawal's L2 block → the vault's payment on NoM. Orders and page loads have no confirmation; their column is the request's own latency.\n\n")
	b.WriteString("| Action | Kind | Count | OK | Reverted | Failed | Skipped | Gas mean | p50 s | p90 s | p99 s | Request p50 ms | p90 ms |\n|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, a := range s.Actions {
		n := a.N
		if a.Kind == "read" {
			n = a.Reads
		}
		gas := ""
		if a.GasMean > 0 {
			gas = n64(int(a.GasMean))
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | %d | %d | %s | %s | %s | %s | %s | %s |\n", a.Name, a.Kind, n64(n), n64(a.OK),
			a.Reverted, a.Failed, a.Skipped, gas, secs(a.Confirm.P50), secs(a.Confirm.P90), secs(a.Confirm.P99),
			msOrBlank(a.Latency.P50), msOrBlank(a.Latency.P90))
	}

	fmt.Fprintf(&b, "\n## Over time (%s windows)\n\nCPU is the mean over that metric's usable observations, in percent of one core; memory is its observed peak. Missing metrics are `n/a`. Exact profile names select the node, primary engine candidates and relayer.\n\n", WindowSize)
	b.WriteString("L2 blocks and momentums require two distinct, ordered sample times with usable nondecreasing counters. Missing observations, counter resets or conflicting times produce `n/a`; a measured zero needs both endpoints. These are observed sample spans within each window, excluding unobserved edges.\n\n")
	b.WriteString("| Window (UTC) | Txs | Tx/s | Failed | Confirm p50 s | p90 s | Load p50 ms | Relayer queue max | L2 blocks | Momentums | node CPU | engine CPU | relayer CPU | node MiB | engine MiB |\n|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, w := range s.Windows {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %s | %s | %.0f | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			w.Start.UTC().Format("15:04"), n64(w.Txs), userRate(w.TxPerSec, w.RateKnown, 2), w.Failed, secs(w.Confirm.P50), secs(w.Confirm.P90), w.Read.P50,
			sampleWindowCount(w.Queued, w.Resources.RelayerQueued > 0), sampleWindowCount(w.L2Blocks, w.L2BlocksKnown), sampleWindowCount(w.L1Moms, w.L1MomsKnown),
			resourcePercent(w.NodeCPU, w.Resources.NodeCPU), resourcePercent(w.EngineCPU, w.Resources.EngineCPU), resourcePercent(w.RelayerCPU, w.Resources.RelayerCPU),
			resourceMemory(w.NodeMem, w.Resources.NodeMem), resourceMemory(w.EngineMem, w.Resources.EngineMem))
	}
	b.WriteString("\n### Resource observations\n\nCounts cover accepted sample rows and each metric's usable observations. They do not establish complete JSONL or whole-run coverage. A present numeric zero can be measured; older captures that already replaced failed parsing with zero cannot be reconstructed.\n\n| Window (UTC) | Samples | Node CPU | Engine CPU | Relayer CPU | Node memory | Engine memory | Relayer queue |\n|---|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, w := range s.Windows {
		c := w.Resources
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %d | %d |\n", w.Start.UTC().Format("15:04"), c.Samples,
			c.NodeCPU, c.EngineCPU, c.RelayerCPU, c.NodeMem, c.EngineMem, c.RelayerQueued)
	}

	if p := s.Phase; p != nil && p.Before != nil && p.After != nil {
		b.WriteString("\n## Storage written over the run\n\nApparent bytes inside each container, after minus before.\n\n| Directory | Before | After | Written | Per confirmed tx |\n|---|---:|---:|---:|---:|\n")
		for _, k := range sortedKeys(p.After.Dirs) {
			before, after := p.Before.Dirs[k].Bytes, p.After.Dirs[k].Bytes
			per := ""
			if t.Confirmed > 0 {
				per = human(float64(after-before) / float64(t.Confirmed))
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", k, human(float64(before)), human(float64(after)), human(float64(after-before)), per)
		}
		if p.Before.Index != nil && p.After.Index != nil {
			d := p.After.Index.FileBytes - p.Before.Index.FileBytes
			fmt.Fprintf(&b, "| index (SQLite file) | %s | %s | %s | %s |\n", human(float64(p.Before.Index.FileBytes)),
				human(float64(p.After.Index.FileBytes)), human(float64(d)), human(float64(d)/float64(max(t.Confirmed, 1))))
		}
	}
	if p := s.Phase; p != nil {
		if heals, ok := p.Data["l1Heals"].([]any); ok && len(heals) > 0 {
			fmt.Fprintf(&b, "\n**L1 gossip gaps healed during the run: %d.** The numbers above include the stalls they caused.\n", len(heals))
		}
	}

	if len(s.Errors) > 0 {
		b.WriteString("\n## Errors\n\n| Action | Error | Count |\n|---|---|---:|\n")
		for i, e := range s.Errors {
			if i == 25 {
				break
			}
			fmt.Fprintf(&b, "| %s | %s | %d |\n", e.Action, strings.ReplaceAll(e.Error, "|", "\\|"), e.N)
		}
	}
	return b.String()
}

func n64[T int | uint64](n T) string { return commas(int64(n)) }

func msOrBlank(v float64) string {
	if v == 0 {
		return ""
	}
	return fmt.Sprintf("%.0f", v)
}

func human(b float64) string {
	neg := b < 0
	if neg {
		b = -b
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	s := fmt.Sprintf("%.1f %s", b, units[i])
	if i == 0 {
		s = fmt.Sprintf("%.0f B", b)
	}
	if neg {
		return "-" + s
	}
	return s
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
