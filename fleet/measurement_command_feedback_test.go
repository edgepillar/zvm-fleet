package fleet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestMeasurementReportRetainsStablePrivateCauseTokens(t *testing.T) {
	cases := []struct {
		name      string
		cause     error
		caller    error
		operation bool
		want      string
	}{
		{"stdout", errMeasurementStdoutLimit, nil, false, "stdout-limit"},
		{"stderr", errMeasurementStderrLimit, nil, false, "stderr-limit"},
		{"caller-cancel", errors.New("sensitive custom cancellation"), context.Canceled, false, "caller-canceled"},
		{"caller-deadline", context.DeadlineExceeded, context.DeadlineExceeded, false, "caller-deadline"},
		{"operation-deadline", context.DeadlineExceeded, nil, true, "operation-timeout"},
		{"legacy-deadline", context.DeadlineExceeded, nil, false, "deadline"},
		{"legacy-cancel", context.Canceled, nil, false, "canceled"},
		{"missing-command", &exec.Error{Name: "sensitive executable", Err: exec.ErrNotFound}, nil, false, "not-found"},
		{"start-refused", &exec.Error{Name: "sensitive executable", Err: exec.ErrDot}, nil, false, "start-failed"},
		{"permission", &os.PathError{Op: "exec", Path: "sensitive path", Err: os.ErrPermission}, nil, false, "permission-denied"},
		{"pipe-wait", exec.ErrWaitDelay, nil, false, "pipe-wait-timeout"},
		{"unknown", errors.New("sensitive unknown failure"), nil, false, "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cause := &measurementCommandError{cause: tc.cause, diagnostic: "sensitive diagnostic", callerState: tc.caller, operationTimedOut: tc.operation}
			got := measurementReportReason(fmt.Errorf("container wrapper: %w", cause))
			if got != "command unavailable ("+tc.want+")" || strings.Contains(got, "sensitive") {
				t.Fatal("retained cause is missing or contains private details", got)
			}
		})
	}
	if measurementReportReason(errors.New("malformed output")) != "malformed output" {
		t.Fatal("non-command diagnostic changed")
	}
}

func TestMeasurementReportJoinsAndOrdersIndependentCommandCauses(t *testing.T) {
	a := &measurementCommandError{cause: errMeasurementStdoutLimit, diagnostic: "private-a"}
	b := &measurementCommandError{cause: errors.Join(errMeasurementStderrLimit, exec.ErrNotFound), diagnostic: "private-b"}
	want := "command unavailable (not-found,stderr-limit,stdout-limit)"
	for _, value := range []error{errors.Join(a, b), errors.Join(b, a, a)} {
		if got := measurementReportReason(value); got != want {
			t.Fatal("joined cause order or deduplication changed", got)
		}
	}
}

type feedbackCyclicError struct{}

func (e *feedbackCyclicError) Error() string { return "private cyclic detail" }
func (e *feedbackCyclicError) Unwrap() error { return e }

func TestMeasurementReportErrorWalkIsBoundedAndPrivate(t *testing.T) {
	for _, value := range []error{&feedbackCyclicError{}, &measurementCommandError{cause: &feedbackCyclicError{}, diagnostic: "private diagnostic"}} {
		if got := measurementReportReason(value); got != "command unavailable (cause-limit)" {
			t.Fatal("error walk was not bounded", got)
		}
	}
}

func TestMeasurementReportActualCommandCauses(t *testing.T) {
	t.Run("nonzero-exit", func(t *testing.T) {
		captureTestDocker(t, "stats", "discarded stdout", "private process diagnostic", "", 23)
		out, err := measurementCommand(context.Background(), "stats")
		var exit *exec.ExitError
		if out != nil || !errors.As(err, &exit) || exit.ExitCode() != 23 || measurementReportReason(err) != "command unavailable (exit-23)" {
			t.Fatal("actual exit was collapsed", err)
		}
	})
	t.Run("operation-timeout", func(t *testing.T) {
		captureTestDocker(t, "stats", "discarded stdout", "private process diagnostic", "block", 0)
		out, err := measurementCommandWithTimeout(context.Background(), 150*time.Millisecond, "stats")
		reason := measurementReportReason(err)
		if out != nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(reason, "operation-timeout") || strings.Contains(reason, "caller-deadline") || strings.Contains(reason, "private") {
			t.Fatal("operation deadline lost its classification", reason)
		}
	})
	t.Run("caller-deadline", func(t *testing.T) {
		captureTestDocker(t, "stats", "discarded stdout", "", "block", 0)
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		out, err := measurementCommandWithTimeout(ctx, time.Second, "stats")
		reason := measurementReportReason(err)
		if out != nil || !strings.Contains(reason, "caller-deadline") || strings.Contains(reason, "operation-timeout") {
			t.Fatal("earlier caller deadline was misclassified", reason)
		}
	})
	t.Run("custom-caller-cancel", func(t *testing.T) {
		calls := captureTestDocker(t, "stats", "discarded stdout", "", "", 0)
		ctx, cancel := context.WithCancelCause(context.Background())
		cause := errors.New("private caller reason")
		cancel(cause)
		out, err := measurementCommand(ctx, "stats")
		if out != nil || !errors.Is(err, cause) || measurementReportReason(err) != "command unavailable (caller-canceled)" {
			t.Fatal("custom caller cancellation was lost or leaked", measurementReportReason(err))
		}
		if _, err := os.Stat(calls); !os.IsNotExist(err) {
			t.Fatal("already canceled command launched")
		}
	})
	t.Run("inherited-pipes", func(t *testing.T) {
		captureTestDocker(t, "stats", "discarded stdout", "", "pipes", 0)
		out, err := measurementCommand(context.Background(), "stats")
		if out != nil || !errors.Is(err, exec.ErrWaitDelay) || !strings.Contains(measurementReportReason(err), "pipe-wait-timeout") {
			t.Fatal("inherited-pipe deadline was collapsed", measurementReportReason(err))
		}
	})
}

func TestMeasurementProbeCopyHasSeparateBoundedPolicy(t *testing.T) {
	for _, tc := range []struct {
		target string
		want   time.Duration
	}{{"cp", 2 * time.Minute}, {"exec", 30 * time.Second}} {
		t.Run(tc.target, func(t *testing.T) {
			captureTestDocker(t, tc.target, "", "private setup diagnostic", "", 23)
			err := captureTestEnvironment().installProbe(context.Background())
			var failure *measurementCommandError
			if !errors.As(err, &failure) || failure.budget != tc.want || failure.callerState != nil || failure.operationTimedOut {
				t.Fatal("probe copy/chmod selected an incorrect budget", err)
			}
			if measurementReportReason(err) != "command unavailable (exit-23)" {
				t.Fatal("setup exit lost its concise cause")
			}
		})
	}
	if measurementCommandTimeout != 30*time.Second || measurementProbeCopyTimeout != 2*time.Minute || measurementCommandStdoutLimit != 1<<20 || measurementCommandStderrLimit != 8<<10 || measurementCommandWaitDelay != 250*time.Millisecond {
		t.Fatal("unrelated command bounds changed")
	}
}

type feedbackWideError struct{ children []error }

func (e *feedbackWideError) Error() string   { return "private wide detail" }
func (e *feedbackWideError) Unwrap() []error { return e.children }

func TestMeasurementReportErrorWalkCountsEmptySlots(t *testing.T) {
	children := make([]error, 256)
	children[255] = &measurementCommandError{cause: errMeasurementStdoutLimit, diagnostic: "private trailing diagnostic"}
	if got := measurementReportReason(&feedbackWideError{children: children}); got != "command unavailable (cause-limit)" {
		t.Fatal("empty joined slots bypassed the cause inspection limit", got)
	}
}
