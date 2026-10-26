package main

// users.go originates here, not in cmd/zvm: the user simulation has no copy in
// the deployment repo.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Zenoglyphs-VM/zvm-fleet/fleet"
	"github.com/Zenoglyphs-VM/zvm-node/apps"
	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

func init() {
	register("users", "simulate users of different profiles for hours: the baseline everything else is compared against", benchUsers)
	register("users-report", "rebuild a user simulation's report from its records", benchUsersReport)
}

func benchUsers(args []string) error {
	fs := flag.NewFlagSet("users", flag.ExitOnError)
	env := bench.Flags(fs)
	deployment := fs.String("deployment", "bench/deployment.json", "the Uniswap deployment written by `zvm bench setup`")
	appsFile := fs.String("apps", "bench/apps.json", "the application deployment written by `zvm apps setup`")
	out := fs.String("out", "bench/users-baseline", "directory for the records and the report")
	probe := fs.String("probe", "bin/zvm-linux", "a linux/amd64 build of the node binary, to size the index before and after")
	follower := fs.String("follower", os.Getenv("ZVM_FOLLOWER_RPC"), "the second node's execution client; empty for none (default $ZVM_FOLLOWER_RPC)")
	heal := fs.Bool("heal", true, "fill L1 gossip gaps while the users run, and record each one")
	profile := fs.String("profile", "dp", "which chain's containers to measure: dp (chain 71), devnet (a zvm-devnet stack) or the deprecated v1 (chain 70)")
	label := fs.String("label", "baseline", "what the report calls this run")
	mix := fs.String("mix", fleet.DefaultMix, "users per profile: "+profileNames())
	users := fs.Int("users", 0, "scale the mix to this many users (0: the mix as given; at most 1,000 per profile)")
	duration := fs.Duration("duration", 6*time.Hour, "how long the users run")
	ramp := fs.Duration("ramp", 30*time.Minute, "users arrive evenly across this")
	sample := fs.Duration("sample", 30*time.Second, "resource and chain sampling interval")
	seed := fs.Uint64("seed", 70, "seed for think times and choices")
	pace := fs.Float64("pace", 1, "multiply every profile's think time (0.1 for a quick smoke test; not a baseline)")
	withdraw := fs.Float64("withdraw", 1, "ZNN a bridger withdraws at a time (small on a chain whose bridge holds little)")
	deposit := fs.Float64("deposit", 1, "ZNN a native user deposits at a time")
	list := fs.Bool("list", false, "print the profiles and exit")
	fs.Parse(args)

	if *list {
		for _, p := range fleet.Profiles {
			fmt.Printf("%-8s %-6s think %-6s  %s\n", p.Name, p.Lane, p.Think, p.Title)
		}
		return nil
	}
	pcs, err := fleet.ParseMix(*mix)
	if err != nil {
		return err
	}
	if *users > 0 {
		pcs = scaleMix(pcs, *users)
	}
	for _, pc := range pcs {
		if pc.Count > 1_000 {
			return fmt.Errorf("%s: %d users; each profile has a range of 1,000 keys", pc.Profile.Name, pc.Count)
		}
		if pc.Profile.Lane == "native" && pc.Count > 100 {
			return fmt.Errorf("native: %d users; only 100 fused Zenon keys are set aside for them", pc.Count)
		}
	}

	e, err := env()
	if err != nil {
		return err
	}
	dex, err := bench.LoadDeployment(*deployment)
	if err != nil {
		return fmt.Errorf("%w — run `zvm bench setup` first", err)
	}
	appsDep, err := apps.Load(*appsFile)
	if err != nil {
		return fmt.Errorf("%w — run `zvm apps setup` first", err)
	}
	arts, err := apps.LoadArtifacts()
	if err != nil {
		return err
	}
	names, ok := fleet.Profile(*profile)
	if !ok {
		return fmt.Errorf("-profile: %q is not v1, v2 or 69", *profile)
	}
	f := &fleet.Env{
		Rollup: e.Cfg, Eth: e.Eth, Submit: e.Submit, L1: e.L1, API: e.API,
		Dex: dex, Apps: appsDep, Arts: arts, Out: *out, Probe: *probe, Repo: ".", Opts: fleet.DefaultOptions(),
		Names: names, Heal: *heal, SampleEvery: *sample, Log: slog.Default().With("svc", "users"),
	}
	if err := f.StartUserRun(); err != nil {
		return err
	}
	if *follower != "" {
		if f.Follower, err = bench.DialPooled(context.Background(), *follower); err != nil {
			return err
		}
	}
	ctx, cancel := signalContext()
	defer cancel()
	if err := f.Init(ctx); err != nil {
		return err
	}
	closeSamples, err := f.StreamSamples()
	if err != nil {
		return err
	}
	defer closeSamples()

	phase := fleet.UsersPhase(fleet.UserOptions{Label: *label, Mix: pcs, Duration: *duration, Ramp: *ramp, Seed: *seed, Pace: *pace, Withdraw: *withdraw, Deposit: *deposit})
	phase.ID = "00-" + phase.ID
	start := time.Now()
	runErr := f.RunPhases(ctx, []fleet.Phase{phase})
	// The report is written even when the run was cut short: it is rebuilt from
	// the records streamed so far.
	if err := finishUsersRun(runErr, closeSamples, *out, filepath.Join(*out, "USERS.md")); err != nil {
		return err
	}
	fmt.Printf("users ran %s → %s\n", time.Since(start).Round(time.Second), filepath.Join(*out, "USERS.md"))
	return nil
}

// Close sampling before replay. A partial report can still be useful after a
// failed run, but must not turn a run, stream or report error into success.
func finishUsersRun(runErr error, closeSamples func() error, in, out string) error {
	streamErr := closeSamples()
	reportErr := writeUsersReport(in, out)
	return errors.Join(runErr, streamErr, reportErr)
}

// scaleMix keeps the mix's proportions at n users, handing the rounding
// remainder out one at a time in profile order.
func scaleMix(pcs []fleet.ProfileCount, n int) []fleet.ProfileCount {
	total := 0
	for _, pc := range pcs {
		total += pc.Count
	}
	out := make([]fleet.ProfileCount, len(pcs))
	given := 0
	for i, pc := range pcs {
		out[i] = pc
		out[i].Count = pc.Count * n / total
		given += out[i].Count
	}
	for i := 0; given < n; i = (i + 1) % len(out) {
		out[i].Count++
		given++
	}
	return out
}

func profileNames() string {
	s := ""
	for i, p := range fleet.Profiles {
		if i > 0 {
			s += ", "
		}
		s += p.Name
	}
	return s
}

func writeUsersReport(in, out string) error {
	s, err := fleet.SummarizeUsers(in)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, []byte(fleet.UsersMarkdown(s)), 0o644); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(in, "summary.json"), raw, 0o644)
}

func benchUsersReport(args []string) error {
	fs := flag.NewFlagSet("users-report", flag.ExitOnError)
	in := fs.String("in", "bench/users-baseline", "directory of a user simulation")
	out := fs.String("out", "", "markdown to write (default: <in>/USERS.md)")
	fs.Parse(args)
	if *out == "" {
		*out = filepath.Join(*in, "USERS.md")
	}
	if err := writeUsersReport(*in, *out); err != nil {
		return err
	}
	fmt.Println("wrote", *out)
	return nil
}
