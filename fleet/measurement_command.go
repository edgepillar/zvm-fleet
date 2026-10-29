package fleet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const measurementCommandStdoutLimit = 1 << 20
const measurementCommandStderrLimit = 8 << 10
const measurementCommandTimeout = 30 * time.Second
const measurementProbeCopyTimeout = 2 * time.Minute
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
	return measurementCommandWithTimeout(parent, measurementCommandTimeout, args...)
}

func measurementCommandWithTimeout(parent context.Context, budget time.Duration, args ...string) ([]byte, error) {
	operationDeadline := time.Now().Add(budget)
	bounded, stop := context.WithDeadline(parent, operationDeadline)
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
		parentDeadline, parentHasDeadline := parent.Deadline()
		operationTimedOut := errors.Is(bounded.Err(), context.DeadlineExceeded) &&
			(!parentHasDeadline || !parentDeadline.Before(operationDeadline))
		return nil, &measurementCommandError{cause: failure, diagnostic: strings.TrimSpace(string(stderr.data)),
			callerState: parent.Err(), operationTimedOut: operationTimedOut, budget: budget}
	}
	return stdout.data, nil
}

// Direct callers can inspect the bounded diagnostic and original error chain.
// Retained sample/snapshot summaries use a concise reason instead of stderr.
type measurementCommandError struct {
	cause             error
	diagnostic        string
	callerState       error
	operationTimedOut bool
	budget            time.Duration
}

func (e *measurementCommandError) Error() string {
	if e.diagnostic != "" {
		return fmt.Sprintf("measurement command: %v; stderr: %s", e.cause, e.diagnostic)
	}
	return fmt.Sprintf("measurement command: %v", e.cause)
}

func (e *measurementCommandError) Unwrap() error { return e.cause }

// Retain stable cause tokens, including joined container failures, without
// copying command diagnostics, arguments or custom cancellation text.
func measurementReportReason(err error) string {
	if err == nil {
		return ""
	}
	codes := map[string]bool{}
	command, deadline, canceled, truncated := false, false, false, false
	remaining := 128
	var visit func(error)
	visit = func(item error) {
		if remaining == 0 {
			truncated = true
			return
		}
		remaining--
		if item == nil {
			return
		}
		if failure, ok := item.(*measurementCommandError); ok {
			command = true
			if failure.callerState == context.Canceled {
				codes["caller-canceled"] = true
			}
			if failure.callerState == context.DeadlineExceeded {
				codes["caller-deadline"] = true
			}
			if failure.operationTimedOut {
				codes["operation-timeout"] = true
			}
		}
		switch item {
		case errMeasurementStdoutLimit:
			codes["stdout-limit"] = true
		case errMeasurementStderrLimit:
			codes["stderr-limit"] = true
		case context.DeadlineExceeded:
			deadline = true
		case context.Canceled:
			canceled = true
		case exec.ErrWaitDelay:
			codes["pipe-wait-timeout"] = true
		case exec.ErrNotFound:
			codes["not-found"] = true
		}
		switch failure := item.(type) {
		case *exec.Error:
			if failure.Err != exec.ErrNotFound {
				codes["start-failed"] = true
			}
		case *exec.ExitError:
			if code := failure.ExitCode(); code >= 0 {
				codes["exit-"+strconv.Itoa(code)] = true
			} else {
				codes["signaled"] = true
			}
		case *os.PathError:
			if os.IsPermission(failure) {
				codes["permission-denied"] = true
			} else if os.IsNotExist(failure) {
				codes["not-found"] = true
			} else {
				codes["start-failed"] = true
			}
		}
		if joined, ok := item.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				if remaining == 0 {
					truncated = true
					break
				}
				visit(child)
			}
		} else if wrapped, ok := item.(interface{ Unwrap() error }); ok {
			visit(wrapped.Unwrap())
		}
	}
	visit(err)
	if truncated {
		codes["cause-limit"] = true
	}
	if !command && !truncated {
		return err.Error()
	}
	if deadline && !codes["caller-deadline"] && !codes["operation-timeout"] {
		codes["deadline"] = true
	}
	if canceled && !codes["caller-canceled"] {
		codes["canceled"] = true
	}
	if len(codes) == 0 {
		codes["other"] = true
	}
	values := make([]string, 0, len(codes))
	for code := range codes {
		values = append(values, code)
	}
	sort.Strings(values)
	return "command unavailable (" + strings.Join(values, ",") + ")"
}
