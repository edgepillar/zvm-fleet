package main

// These are unexported helpers in the monorepo's cmd/zvm,
// shared by the node, the relayer and the bridge. They live in zvm-core/service
// now; these aliases keep run.go close to its cmd/zvm original.

import (
	"context"

	"github.com/Zenoglyphs-VM/zvm-core/cli"
	"github.com/Zenoglyphs-VM/zvm-core/service"
)

func signalContext() (context.Context, context.CancelFunc) { return service.SignalContext() }

func waitFor[T any](ctx context.Context, what string, fn func() (T, error)) (T, error) {
	return service.WaitFor(ctx, what, fn)
}

var (
	writeJSON   = cli.WriteJSON
	parseUnits  = cli.ParseUnits
	formatUnits = cli.FormatUnits
)
