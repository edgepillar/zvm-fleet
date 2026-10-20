package main

// run.go is a copy of cmd/zvm/fleet.go in the zvm deployment repo, which is
// still the source of truth, with the bench.Env rename applied and the
// subcommands registered against this binary instead of `zvm bench`.

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Zenoglyphs-VM/zvm-fleet/fleet"
	"github.com/Zenoglyphs-VM/zvm-node/apps"
	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

// benchFleet runs every service at once across as many wallets as the chain
// funds, with storage and resources measured around each phase.
func init() {
	register("fleet", "run every service at once across as many wallets as the chain funds, measuring each phase", benchFleet)
	register("report", "rebuild the tables from a finished run's phase results", benchFleetReport)
	register("l1-heal", "fill L1 gossip gaps once and report what it did", benchL1Heal)
}

func benchFleet(args []string) error {
	fs := flag.NewFlagSet("bench fleet", flag.ExitOnError)
	env := bench.Flags(fs)
	o := fleet.DefaultOptions()
	deployment := fs.String("deployment", "bench/deployment.json", "the Uniswap deployment written by `zvm bench setup`")
	appsFile := fs.String("apps", "bench/apps.json", "the application deployment written by `zvm apps setup`")
	out := fs.String("out", "bench/fleet", "directory for phase results")
	probe := fs.String("probe", "bin/zvm-linux", "a linux/amd64 build of this binary, run inside the node containers to size their databases")
	follower := fs.String("follower", os.Getenv("ZVM_FOLLOWER_RPC"), "the second node's execution client; empty for none (default $ZVM_FOLLOWER_RPC)")
	heal := fs.Bool("heal", true, "fill L1 gossip gaps between the RPC node and the pillar while phases run, and record each one")
	profile := fs.String("profile", "dp", "which chain's containers to measure: dp (chain 71, dynamic plasma, no second node), devnet (a zvm-devnet stack) or the deprecated v1 (chain 70)")
	only := fs.String("phases", "all", "comma-separated phase ids or fragments of them, or all")
	list := fs.Bool("list", false, "print the phases and exit")
	scale := fs.String("scale", "1000,10000,100000", "EVM wallet counts for the scaling phases")
	fs.Float64Var(&o.ScaleRate, "scale-rate", o.ScaleRate, "offered tx/s in the scaling phases")
	fs.Float64Var(&o.SaturateRate, "saturate-rate", o.SaturateRate, "offered tx/s in the saturation phase")
	fs.IntVar(&o.AppWallets, "app-wallets", o.AppWallets, "wallets per application workload")
	fs.Float64Var(&o.AppRate, "app-rate", o.AppRate, "offered tx/s for application workloads")
	fs.IntVar(&o.Native, "native", o.Native, "native wallets to onboard and drive")
	fs.Float64Var(&o.NativeRate, "native-rate", o.NativeRate, "offered tx/s on the native lane")
	fs.IntVar(&o.Deposits, "deposits", o.Deposits, "bridge deposits")
	fs.IntVar(&o.Withdrawals, "withdrawals", o.Withdrawals, "bridge withdrawals")
	fs.IntVar(&o.Drips, "drips", o.Drips, "faucet requests")
	fs.IntVar(&o.Uploads, "uploads", o.Uploads, "media uploads")
	fs.IntVar(&o.Verifies, "verifies", o.Verifies, "concurrent verification jobs")
	fs.DurationVar(&o.Duration, "duration", o.Duration, "default run length")
	fs.DurationVar(&o.APIEach, "api-each", o.APIEach, "per endpoint and concurrency in the API ladder")
	fs.Parse(args)

	o.Scale = nil
	for _, s := range strings.Split(*scale, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n <= 0 {
			return fmt.Errorf("-scale: %q is not a wallet count", s)
		}
		o.Scale = append(o.Scale, n)
	}
	phases := fleet.Catalog(o)
	if *only != "all" {
		var picked []fleet.Phase
		for _, ph := range phases {
			for _, want := range strings.Split(*only, ",") {
				if want = strings.TrimSpace(want); want != "" && strings.Contains(ph.ID, want) {
					picked = append(picked, ph)
					break
				}
			}
		}
		phases = picked
	}
	if *list {
		for _, ph := range phases {
			fmt.Printf("%-22s %s\n", ph.ID, ph.Title)
		}
		return nil
	}
	if len(phases) == 0 {
		return fmt.Errorf("no phase matches %q", *only)
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
		Dex: dex, Apps: appsDep, Arts: arts, Out: *out, Probe: *probe, Repo: ".", Opts: o,
		Names: names, Heal: *heal, Log: slog.Default().With("svc", "fleet"),
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
	start := time.Now()
	if err := f.RunPhases(ctx, phases); err != nil {
		return err
	}
	fmt.Printf("%d phases in %s → %s\n", len(phases), time.Since(start).Round(time.Second), *out)
	return nil
}

func benchFleetReport(args []string) error {
	fs := flag.NewFlagSet("bench fleet-report", flag.ExitOnError)
	in := fs.String("in", "bench/fleet", "directory of phase results")
	out := fs.String("out", "bench/FLEET.md", "markdown to write")
	fs.Parse(args)
	md, n, err := fleet.Report(*in)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s from %d phases\n", *out, n)
	return nil
}

// benchL1Heal fills L1 gossip gaps once and reports what it did: the fleet
// healer, for use by hand after a devnet stalls (docs/HALT.md).
func benchL1Heal(args []string) error {
	fs := flag.NewFlagSet("bench l1-heal", flag.ExitOnError)
	env := bench.Flags(fs)
	profile := fs.String("profile", "dp", "which chain's pillar to repair: dp (chain 71), devnet (a zvm-devnet stack) or the deprecated v1 (chain 70)")
	native := fs.Int("native", fleet.DefaultOptions().Native, "native wallets to check as well")
	fs.Parse(args)
	names, ok := fleet.Profile(*profile)
	if !ok {
		return fmt.Errorf("-profile: %q is not v1, v2 or 69", *profile)
	}
	e, err := env()
	if err != nil {
		return err
	}
	o := fleet.DefaultOptions()
	o.Native = *native
	f := &fleet.Env{Rollup: e.Cfg, Eth: e.Eth, Submit: e.Submit, L1: e.L1, API: e.API, Names: names, Opts: o,
		Log: slog.Default().With("svc", "l1-heal")}
	events, err := f.HealOnce(context.Background())
	if err != nil {
		return err
	}
	if len(events) == 0 {
		fmt.Println("no gaps: the pillar holds everything the RPC node does")
		return nil
	}
	for _, ev := range events {
		fmt.Printf("%s heights %d..%d: sent %d, %d momentums behind %s\n", ev.Address, ev.From, ev.To, ev.Sent, ev.Lag, ev.Error)
	}
	return nil
}
