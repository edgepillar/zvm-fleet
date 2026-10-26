package fleet

// SampleKnown records whether a numeric sample field was actually read and
// parsed. It does not assert service health or chain finality. A nil Known on
// a legacy Sample leaves zero ambiguous; positive legacy fields remain usable.
type SampleKnown struct {
	L2       bool `json:"l2"`
	Indexed  bool `json:"indexed"`
	Follower bool `json:"follower"`
	L1       bool `json:"l1"`
	Derived  bool `json:"derived"`
}

func (s *Sample) captureInfo(info map[string]any) {
	if s.Known == nil {
		s.Known = new(SampleKnown)
	}
	s.Relayer, _ = info["relayer"].(map[string]any)
	delete(s.Relayer, "accounts")
	s.Bridge, _ = info["bridge"].(map[string]any)
	s.Faucet = nil
	if f, ok := info["faucet"].(map[string]any); ok {
		s.Faucet = pick(f, "served", "l1Balances", "l2Balance")
	}
	l2, _ := info["l2"].(map[string]any)
	l1, _ := info["l1"].(map[string]any)
	d, _ := info["derivation"].(map[string]any)
	s.L2, s.Known.L2 = counterValue(l2["head"])
	s.L1, s.Known.L1 = counterValue(l1["frontier"])
	s.Derived, s.Known.Derived = counterValue(d["momentum"])
}

// sampleCounterWindow accepts only one ordered, nondecreasing observation
// segment. Missing observations or a reset make the entire window unavailable;
// a later recovery cannot reconstruct what happened across that gap.
type sampleCounterWindow struct {
	first, last uint64
	lastTime    int64
	seen, pair  bool
	invalid     bool
}

func (w *sampleCounterWindow) observe(at int64, value uint64, known bool) {
	if !known {
		w.invalid = true
		return
	}
	if !w.seen {
		w.first, w.last, w.lastTime, w.seen = value, value, at, true
		return
	}
	if at < w.lastTime || value < w.last {
		w.invalid = true
		return
	}
	if at == w.lastTime {
		if value != w.last {
			w.invalid = true
		}
		return
	}
	w.last, w.lastTime, w.pair = value, at, true
}

func (w *sampleCounterWindow) result() (uint64, bool) {
	if w.invalid || !w.pair {
		return 0, false
	}
	return w.last - w.first, true
}

func sampleWindowCount(value uint64, known bool) string {
	if !known {
		return "n/a"
	}
	return unsignedCommas(value)
}
