package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestMeasurementCommandActiveDeadlinePreservesFailure(t *testing.T) {
	for _, tc := range captureTestCases() {
		if !tc.hasError {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			calls := captureTestDocker(t, tc.target, tc.stdout, "", "block", 0)
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			usable, err := tc.call(ctx, captureTestEnvironment())
			if usable || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("active command deadline became usable or lost its identity", err)
			}
			if raw, err := os.ReadFile(calls); err != nil || len(raw) == 0 {
				t.Fatal("fixture did not enter the actual command", err)
			}
		})
	}
}

func TestMeasurementCommandOverLimitCancelsActiveProcess(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			stdout, stderr := `{"fileBytes":17}`, ""
			expected := errMeasurementStdoutLimit
			if stream == "stdout" {
				stdout += strings.Repeat(" ", captureTestStdoutLimit+1-len(stdout))
			} else {
				stderr = strings.Repeat("x", captureTestStderrLimit+1)
				expected = errMeasurementStderrLimit
			}
			captureTestDocker(t, "exec", stdout, stderr, "block", 0)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			start := time.Now()
			report, file, pages, err := captureTestEnvironment().probeObserved(ctx, "synthetic-node", false)
			if !errors.Is(err, expected) || report != nil || file || pages {
				t.Fatal("active output limit lost its reason or retained data", err)
			}
			if time.Since(start) >= 1500*time.Millisecond {
				t.Fatal("output limit waited for the self-expiring process")
			}
			if errors.Is(err, context.Canceled) {
				t.Fatal("internal output stop invented caller cancellation")
			}
		})
	}
}

func TestMeasurementDirectoryCommandRetainsPartialIndependentCapture(t *testing.T) {
	captureTestDocker(t, "exec", "/data 17 1\n/data/media 0 0\n", "synthetic directory diagnostic", "", 0)
	t.Setenv("FLEET_CAPTURE_FAIL_CONTAINER", "synthetic-other")
	e := captureTestEnvironment()
	e.Names.NodeB = "synthetic-other"
	e.running["synthetic-other"] = true
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := e.dirSizesResult(ctx)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 || !strings.Contains(err.Error(), "synthetic directory diagnostic") {
		t.Fatal("directory failure lost its process identity or diagnostic", err)
	}
	if len(rows) != 2 || rows["synthetic-node:/data"].Bytes != 17 || !rows["synthetic-node:/data"].Known["bytes"] {
		t.Fatal("failed container discarded independent complete observations")
	}
	if _, present := rows["synthetic-other:/data"]; present {
		t.Fatal("failed directory command contributed a measurement")
	}
}

func TestMeasurementDirectoryAlreadyCanceledKeepsErrorAndNoOutput(t *testing.T) {
	calls := captureTestDocker(t, "exec", "/data 17 1\n", "", "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rows, err := captureTestEnvironment().dirSizesResult(ctx)
	if len(rows) != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal("directory cancellation lost its identity or retained measurements", err)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatal("canceled directory command was launched")
	}
}

func TestMeasurementCommandSampleKeepsConcisePrivateDiagnosticBoundary(t *testing.T) {
	captureTestDocker(t, "stats", resourceDockerRow("synthetic-node", nil), "synthetic-sensitive-command-output", "", 23)
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/blocks" {
			io.WriteString(out, "[]")
		} else {
			io.WriteString(out, "{}")
		}
	}))
	defer server.Close()
	e := captureTestEnvironment()
	e.API, e.http = server.URL, server.Client()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	sample := e.sampleOnce(ctx)
	if len(sample.Errors) != 1 || sample.Errors[0] != "docker stats: command unavailable" {
		t.Fatal("sample replaced its concise command failure summary", sample.Errors)
	}
	wire, err := json.Marshal(sample)
	if err != nil || strings.Contains(string(wire), "synthetic-sensitive-command-output") {
		t.Fatal("sample retained raw command diagnostics", err)
	}
	_, direct := e.dockerStatsResult(ctx)
	var exit *exec.ExitError
	if !errors.As(direct, &exit) || exit.ExitCode() != 23 || !strings.Contains(direct.Error(), "synthetic-sensitive-command-output") {
		t.Fatal("direct error lost its separate bounded diagnostic or process status", direct)
	}
}
