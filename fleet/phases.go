package fleet

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

func short(n int) string {
	switch {
	case n >= 1_000_000 && n%1_000_000 == 0:
		return fmt.Sprintf("%dm", n/1_000_000)
	case n >= 1_000 && n%1_000 == 0:
		return fmt.Sprintf("%dk", n/1_000)
	}
	return fmt.Sprint(n)
}

// Catalog is every phase, in the order they run. Later phases lean on earlier
// ones: the buy phase buys what the list phase listed, and the native phases
// use the wallets onboarding fused.
func Catalog(o Options) []Phase {
	maxScale := slices.Max(o.Scale)
	var ps []Phase
	add := func(name, title string, run func(ctx context.Context, e *Env, p *PhaseRecord) error) {
		ps = append(ps, Phase{ID: fmt.Sprintf("%02d-%s", len(ps), name), Title: title, Run: run})
	}

	add("api-baseline", "Explorer API, before load", func(ctx context.Context, e *Env, p *PhaseRecord) error {
		p.set("ladder", e.ladder(ctx, e.endpoints(ctx), []int{1, 16, 64}, o.APIEach))
		return nil
	})
	add("fund", fmt.Sprintf("Fund %s EVM wallets", short(maxScale)), func(ctx context.Context, e *Env, p *PhaseRecord) error {
		_, err := e.run(ctx, p, bench.RunConfig{Scenario: "fund", Custom: noop, Lane: "batch", Wallets: maxScale,
			Fund: wei(0.5), Duration: time.Nanosecond, Drain: time.Second})
		return err
	})
	for _, w := range o.Scale {
		dur := time.Duration(float64(w) / o.ScaleRate * float64(time.Second))
		dur = min(max(dur, o.Duration), 9*time.Minute)
		add("wallets-"+short(w), fmt.Sprintf("Transfers to new addresses from %s wallets at %g tx/s", short(w), o.ScaleRate),
			func(ctx context.Context, e *Env, p *PhaseRecord) error {
				_, err := e.run(ctx, p, bench.RunConfig{Scenario: "transfer-new", Lane: "batch", Wallets: w,
					Rate: o.ScaleRate, Duration: dur, Fund: wei(0.5)})
				return err
			})
	}
	add("saturate", fmt.Sprintf("Batch lane offered %g tx/s from %s wallets", o.SaturateRate, short(maxScale)),
		func(ctx context.Context, e *Env, p *PhaseRecord) error {
			_, err := e.run(ctx, p, bench.RunConfig{Scenario: "transfer-new", Lane: "batch", Wallets: maxScale,
				Rate: o.SaturateRate, Duration: 3 * time.Minute, Drain: 10 * time.Minute, Fund: wei(0.5)})
			return err
		})

	appRun := func(ctx context.Context, e *Env, p *PhaseRecord, sc *bench.Scenario, base uint32, d time.Duration) error {
		_, err := e.run(ctx, p, bench.RunConfig{Scenario: sc.Name, Custom: sc, Lane: "batch", Wallets: o.AppWallets,
			WalletBase: base, Rate: o.AppRate, Duration: d, Fund: wei(3)})
		return err
	}
	add("app-swap", "Swaps of ZNN for a token through Uniswap V2", func(ctx context.Context, e *Env, p *PhaseRecord) error {
		sc, err := e.swapScenario(ctx)
		if err != nil {
			return err
		}
		return appRun(ctx, e, p, sc, swapBase, o.Duration)
	})
	add("app-nft-mint", "NFT mints from a clone collection's public drop", func(ctx context.Context, e *Env, p *PhaseRecord) error {
		sc, err := e.mintScenario(ctx)
		if err != nil {
			return err
		}
		return appRun(ctx, e, p, sc, mintBase, o.Duration)
	})
	add("app-nft-list", "Marketplace listings", func(ctx context.Context, e *Env, p *PhaseRecord) error {
		sc, err := e.listScenario(ctx, sellerBase)
		if err != nil {
			return err
		}
		return appRun(ctx, e, p, sc, sellerBase, o.Duration)
	})
	add("app-nft-buy", "Marketplace purchases, each listing once", func(ctx context.Context, e *Env, p *PhaseRecord) error {
		sc, open, err := e.buyScenario(ctx)
		if err != nil {
			return err
		}
		p.set("openListings", open)
		d := min(o.Duration, time.Duration(float64(open)/o.AppRate*float64(time.Second))+15*time.Second)
		return appRun(ctx, e, p, sc, buyerBase, d)
	})
	add("app-farm", "Farm deposits and harvests", func(ctx context.Context, e *Env, p *PhaseRecord) error {
		sc, err := e.farmScenario(ctx)
		if err != nil {
			return err
		}
		return appRun(ctx, e, p, sc, farmBase, o.Duration)
	})

	add("native-onboard", fmt.Sprintf("Fuse plasma for %s new Zenon accounts", short(o.Native)),
		func(ctx context.Context, e *Env, p *PhaseRecord) error {
			return e.onboardNative(ctx, p, o.Native, 50)
		})
	nativeRun := func(ctx context.Context, e *Env, p *PhaseRecord, sc *bench.Scenario, rate float64) error {
		_, err := e.run(ctx, p, bench.RunConfig{Scenario: sc.Name, Custom: sc, Lane: "native", Wallets: o.Native,
			NativeBase: nativeBase, NativeLimit: o.Native, Rate: rate, Duration: o.Duration, Drain: 8 * time.Minute, Fund: wei(0.5)})
		return err
	}
	add("native-transfer", fmt.Sprintf("Native lane: transfers from %s Zenon accounts at %g tx/s", short(o.Native), o.NativeRate),
		func(ctx context.Context, e *Env, p *PhaseRecord) error {
			arts := e.Arts
			sc := &bench.Scenario{Name: "transfer-new", Gas: 21_000, Build: bench.TransferNew}
			_ = arts
			return nativeRun(ctx, e, p, sc, o.NativeRate)
		})
	add("native-nft-mint", "Native lane: NFT mints", func(ctx context.Context, e *Env, p *PhaseRecord) error {
		sc, err := e.mintScenario(ctx)
		if err != nil {
			return err
		}
		return nativeRun(ctx, e, p, sc, 6)
	})

	add("bridge-deposit", fmt.Sprintf("%s vault deposits from 200 Zenon accounts", short(o.Deposits)),
		func(ctx context.Context, e *Env, p *PhaseRecord) error { return e.bridgeDeposits(ctx, p, o.Deposits) })
	add("bridge-withdraw", fmt.Sprintf("%s withdrawals, one per EVM wallet", short(o.Withdrawals)),
		func(ctx context.Context, e *Env, p *PhaseRecord) error {
			return e.bridgeWithdrawals(ctx, p, o.Withdrawals)
		})
	add("faucet", fmt.Sprintf("%d faucet requests for fresh addresses", o.Drips),
		func(ctx context.Context, e *Env, p *PhaseRecord) error { return e.faucetBurst(ctx, p, o.Drips, 8) })
	add("media", fmt.Sprintf("%d media uploads from 100 signed sessions", o.Uploads),
		func(ctx context.Context, e *Env, p *PhaseRecord) error { return e.mediaLoad(ctx, p, o.Uploads) })
	add("verify", fmt.Sprintf("%d contract verifications at once", o.Verifies),
		func(ctx context.Context, e *Env, p *PhaseRecord) error { return e.verifyLoad(ctx, p, o.Verifies) })

	add("api-final", "Explorer API, after everything", func(ctx context.Context, e *Env, p *PhaseRecord) error {
		p.set("ladder", e.ladder(ctx, e.endpoints(ctx), []int{1, 16, 64}, o.APIEach))
		return nil
	})
	add("api-under-load", "Heavy API readers during a batch-lane run", func(ctx context.Context, e *Env, p *PhaseRecord) error {
		var heavy []Endpoint
		for _, ep := range e.endpoints(ctx) {
			if ep.Heavy {
				heavy = append(heavy, ep)
			}
		}
		lctx, cancel := context.WithCancel(ctx)
		done := make(chan LoadRow)
		go func() { done <- e.load(lctx, heavy, 32, 0) }()
		_, err := e.run(ctx, p, bench.RunConfig{Scenario: "transfer-new", Lane: "batch", Wallets: o.AppWallets,
			WalletBase: scaleBase, Rate: o.ScaleRate, Duration: o.Duration, Fund: wei(0.5)})
		cancel()
		p.set("readers", <-done)
		return err
	})
	add("follower", "The second node: agreement, flush and recovery", func(ctx context.Context, e *Env, p *PhaseRecord) error {
		return e.followerCheck(ctx, p)
	})
	return ps
}
