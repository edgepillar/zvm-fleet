package fleet

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const measurementCommandStdoutLimit = 1 << 20
const measurementCommandStderrLimit = 8 << 10
const measurementCommandTimeout = 30 * time.Second
const measurementCommandWaitDelay = 250 * time.Millisecond

var errMeasurementStdoutLimit = errors.New("measurement stdout exceeds 1048576 bytes")
var errMeasurementStderrLimit = errors.New("measurement stderr exceeds 8192 bytes")

// Each stream has one os/exec copier. CancelCauseFunc is safe for both copiers;
// the buffers and overflow flags are read only after Run has joined them.
type measurementCommandBuffer struct {
	data     []byte
	limit    int
	overflow bool
	cancel   context.CancelCauseFunc
	cause    error
}

func (b *measurementCommandBuffer) Write(p []byte) (int, error) {
	keep := min(len(p), b.limit-len(b.data))
	b.data = append(b.data, p[:keep]...)
	if keep != len(p) {
		b.overflow = true
		b.cancel(b.cause)
	}
	// Continue draining while cancellation terminates the direct command.
	// Retained bytes never exceed the per-stream limit.
	return len(p), nil
}

// measurementCommand captures Docker's measurement/setup commands, with an
// earlier caller deadline taking precedence. It does not control a daemon's
// remote operation or kill unrelated/descendant processes.
func measurementCommand(parent context.Context, args ...string) ([]byte, error) {
	bounded, stop := context.WithTimeout(parent, measurementCommandTimeout)
	defer stop()
	ctx, cancel := context.WithCancelCause(bounded)
	defer cancel(nil)
	stdout := measurementCommandBuffer{limit: measurementCommandStdoutLimit, cancel: cancel, cause: errMeasurementStdoutLimit}
	stderr := measurementCommandBuffer{limit: measurementCommandStderrLimit, cancel: cancel, cause: errMeasurementStderrLimit}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = measurementCommandWaitDelay
	failure := cmd.Run()
	// Keep process status, caller cancellation/custom cause and policy timeout
	// independently inspectable. Internal output cancellation does not invent
	// caller cancellation. A limit on either stream invalidates all stdout.
	failure = errors.Join(failure, context.Cause(ctx), bounded.Err())
	if stdout.overflow {
		failure = errors.Join(failure, errMeasurementStdoutLimit)
	}
	if stderr.overflow {
		failure = errors.Join(failure, errMeasurementStderrLimit)
	}
	if failure != nil {
		return nil, &measurementCommandError{cause: failure, diagnostic: strings.TrimSpace(string(stderr.data))}
	}
	return stdout.data, nil
}

// Direct callers can inspect the bounded diagnostic and original error chain.
// Retained sample/snapshot summaries use a concise reason instead of stderr.
type measurementCommandError struct {
	cause      error
	diagnostic string
}

func (e *measurementCommandError) Error() string {
	if e.diagnostic != "" {
		return fmt.Sprintf("measurement command: %v; stderr: %s", e.cause, e.diagnostic)
	}
	return fmt.Sprintf("measurement command: %v", e.cause)
}

func (e *measurementCommandError) Unwrap() error { return e.cause }

func measurementReportReason(err error) string {
	var failure *measurementCommandError
	if errors.As(err, &failure) {
		return "command unavailable"
	}
	return err.Error()
}
