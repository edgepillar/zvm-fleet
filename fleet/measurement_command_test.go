package fleet

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const captureTestStdoutLimit = 1 << 20
const captureTestStderrLimit = 8 << 10

// The fixture emits finite retained input and never invokes a Docker daemon.
func captureTestDocker(t *testing.T, target, stdout, stderr, mode string, status int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("synthetic command fixture uses a POSIX shell")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	for name, raw := range map[string]string{"stdout": stdout, "stderr": stderr} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FLEET_CAPTURE_CALLS"
if [ "$1" != "$FLEET_CAPTURE_TARGET" ]; then exit 0; fi
if [ -n "$FLEET_CAPTURE_FAIL_CONTAINER" ] && [ "$2" = "$FLEET_CAPTURE_FAIL_CONTAINER" ]; then
    /bin/cat "$FLEET_CAPTURE_STDERR" >&2
    exit 23
fi
/bin/cat "$FLEET_CAPTURE_STDOUT"
/bin/cat "$FLEET_CAPTURE_STDERR" >&2
if [ "$FLEET_CAPTURE_MODE" = "pipes" ]; then /bin/sleep 2 & exit 0; fi
if [ "$FLEET_CAPTURE_MODE" = "block" ]; then exec /bin/sleep 2; fi
exit "$FLEET_CAPTURE_STATUS"
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for key, value := range map[string]string{
		"FLEET_CAPTURE_TARGET": target, "FLEET_CAPTURE_CALLS": calls,
		"FLEET_CAPTURE_STDOUT": filepath.Join(dir, "stdout"), "FLEET_CAPTURE_STDERR": filepath.Join(dir, "stderr"),
		"FLEET_CAPTURE_MODE": mode, "FLEET_CAPTURE_STATUS": string(rune('0' + status)), "FLEET_CAPTURE_FAIL_CONTAINER": "",
	} {
		t.Setenv(key, value)
	}
	// The two fixture exit codes are zero and 23.
	if status == 23 {
		t.Setenv("FLEET_CAPTURE_STATUS", "23")
	}
	return calls
}

type captureTestCase struct {
	name, target, stdout string
	hasError             bool
	call                 func(context.Context, *Env) (bool, error)
}

func captureTestCases() []captureTestCase {
	return []captureTestCase{
		{"discovery", "ps", "synthetic-node\n", true, func(ctx context.Context, e *Env) (bool, error) {
			rows, err := runningContainers(ctx)
			return len(rows) != 0, err
		}},
		{"statistics", "stats", resourceDockerRow("synthetic-node", nil), true, func(ctx context.Context, e *Env) (bool, error) {
			rows, err := e.dockerStatsResult(ctx)
			return len(rows) != 0, err
		}},
		{"directories", "exec", "/data 17 1\n/data/media 0 0\n", false, func(ctx context.Context, e *Env) (bool, error) {
			return len(e.dirSizes(ctx)) != 0, nil
		}},
		{"storage", "exec", `{"fileBytes":17}`, true, func(ctx context.Context, e *Env) (bool, error) {
			report, file, pages, err := e.probeObserved(ctx, "synthetic-node", false)
			return report != nil || file || pages, err
		}},
		{"install-copy", "cp", "", true, func(ctx context.Context, e *Env) (bool, error) {
			err := e.installProbe(ctx)
			return err == nil, err
		}},
		{"install-mode", "exec", "", true, func(ctx context.Context, e *Env) (bool, error) {
			err := e.installProbe(ctx)
			return err == nil, err
		}},
	}
}

func captureTestEnvironment() *Env {
	return &Env{Names: Names{Node: "synthetic-node"}, Probe: "synthetic-probe", running: map[string]bool{"synthetic-node": true}}
}

func TestMeasurementCommandRejectsOversizedCapture(t *testing.T) {
	for _, tc := range captureTestCases() {
		for _, stream := range []string{"stdout", "stderr"} {
			t.Run(tc.name+"/"+stream, func(t *testing.T) {
				stdout, stderr := tc.stdout, ""
				if stream == "stdout" {
					stdout += strings.Repeat(" ", captureTestStdoutLimit+1-len(stdout))
				} else {
					stderr = strings.Repeat("x", captureTestStderrLimit+1)
				}
				captureTestDocker(t, tc.target, stdout, stderr, "", 0)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				usable, err := tc.call(ctx, captureTestEnvironment())
				if usable || tc.hasError && err == nil {
					t.Fatal("over-limit command became a usable observation or installation")
				}
				if err != nil && len(err.Error()) > captureTestStderrLimit+1024 {
					t.Fatal("diagnostic retained more than its bounded prefix")
				}
			})
		}
	}
}

func TestMeasurementCommandPreservesCancellation(t *testing.T) {
	for _, tc := range captureTestCases() {
		if !tc.hasError {
			continue
		}
		for _, mode := range []string{"canceled", "cause", "deadline"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				calls := captureTestDocker(t, tc.target, tc.stdout, "", "", 0)
				parent := context.Background()
				cause := errors.New("synthetic caller cancellation")
				var ctx context.Context
				var expected error
				if mode == "deadline" {
					child, cancel := context.WithTimeout(parent, time.Nanosecond)
					defer cancel()
					<-child.Done()
					ctx, expected = child, context.DeadlineExceeded
				} else {
					child, cancel := context.WithCancelCause(parent)
					if mode == "cause" {
						cancel(cause)
					} else {
						cancel(nil)
					}
					ctx, expected = child, context.Canceled
				}
				usable, err := tc.call(ctx, captureTestEnvironment())
				if usable || !errors.Is(err, expected) || mode == "cause" && !errors.Is(err, cause) {
					t.Fatal("command wrapper discarded caller cancellation, deadline or cause", err)
				}
				if _, err := os.Stat(calls); !os.IsNotExist(err) {
					t.Fatal("already interrupted command was launched")
				}
			})
		}
	}
}

func TestMeasurementCommandRetainsExitStatus(t *testing.T) {
	for _, tc := range captureTestCases() {
		if !tc.hasError {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			captureTestDocker(t, tc.target, tc.stdout, "synthetic command diagnostic", "", 23)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			usable, err := tc.call(ctx, captureTestEnvironment())
			var exit *exec.ExitError
			if usable || !errors.As(err, &exit) || exit.ExitCode() != 23 || !strings.Contains(err.Error(), "synthetic command diagnostic") {
				t.Fatal("command wrapper discarded process exit identity or bounded diagnostic", err)
			}
		})
	}
}

func TestMeasurementCommandBoundsInheritedPipes(t *testing.T) {
	captureTestDocker(t, "exec", `{"fileBytes":17}`, "", "pipes", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	report, file, pages, err := captureTestEnvironment().probeObserved(ctx, "synthetic-node", false)
	if !errors.Is(err, exec.ErrWaitDelay) || report != nil || file || pages {
		t.Fatal("descendant-held output pipes became a completed storage observation", err)
	}
	if time.Since(start) >= 1500*time.Millisecond {
		t.Fatal("probe waited for the fixture descendant instead of bounding pipe drain")
	}
}

func TestMeasurementCommandExactCaptureBoundariesRemainUsable(t *testing.T) {
	for _, tc := range captureTestCases() {
		for _, stream := range []string{"stdout", "stderr"} {
			t.Run(tc.name+"/"+stream, func(t *testing.T) {
				stdout, stderr := tc.stdout, ""
				if stream == "stdout" {
					stdout += strings.Repeat(" ", captureTestStdoutLimit-len(stdout))
				} else {
					stderr = strings.Repeat("x", captureTestStderrLimit)
				}
				captureTestDocker(t, tc.target, stdout, stderr, "", 0)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				usable, err := tc.call(ctx, captureTestEnvironment())
				if !usable || err != nil {
					t.Fatal("complete boundary-sized command was lost", err)
				}
			})
		}
	}
}
