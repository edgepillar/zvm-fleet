package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type userInputFailureReader struct{}

func (userInputFailureReader) Read([]byte) (int, error) {
	return 0, fmt.Errorf("synthetic read failure")
}

func TestUserJSONLReadFailuresCannotBecomePartialSuccess(t *testing.T) {
	reader := io.MultiReader(strings.NewReader(userJSONLRecordA+"\n"), userInputFailureReader{})
	_, err := scanUserJSONL(reader, "records.jsonl", func(raw []byte) error { _, err := userRecord(raw); return err })
	if err == nil {
		t.Fatal("read failure after a valid prefix became success")
	}
}

func TestUserJSONLCRLFAndUnorderedRecordsKeepOriginalBytes(t *testing.T) {
	records := userJSONLRecordB + "\r\n" + userJSONLRecordA + "\r\n"
	dir := userJSONLFixture(t, records, userJSONLSampleA+"\r\n"+userJSONLSampleB+"\r\n")
	summary, err := SummarizeUsers(dir)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(records))
	if summary.Inputs.Records.Rows != 2 || summary.Inputs.Records.UsedRows != 2 || summary.Inputs.Records.Bytes != int64(len(records)) ||
		summary.Inputs.Records.SHA256 != hex.EncodeToString(digest[:]) || summary.Inputs.Records.State != "terminated" || !summary.Inputs.Records.Stable {
		t.Fatal("CRLF/order processing changed the raw-input evidence")
	}
	if summary.Totals.Reads != 2 || summary.Inputs.Partial || summary.Inputs.RunCompletion != "not-established" {
		t.Fatal("input EOF or row order was confused with whole-run completion")
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "records.jsonl")); err != nil || string(raw) != records {
		t.Fatal("record replay normalized the original input")
	}
}

func TestUserJSONLMissingSamplesRemainExplicit(t *testing.T) {
	dir := userJSONLFixture(t, userJSONLRecordA+"\n", "")
	if err := os.Remove(filepath.Join(dir, "samples.jsonl")); err != nil {
		t.Fatal(err)
	}
	summary, err := SummarizeUsers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Inputs.Partial || summary.Inputs.Samples.State != "missing" || summary.Inputs.Samples.Rows != 0 {
		t.Fatal("absent sampling evidence became a complete input")
	}
	if !strings.Contains(UsersMarkdown(summary), "PARTIAL INPUT") {
		t.Fatal("missing samples were hidden from the report")
	}
}

func TestUserJSONLInputMutationIsRefused(t *testing.T) {
	dir := userJSONLFixture(t, userJSONLRecordA+"\n", "")
	path := filepath.Join(dir, "records.jsonl")
	changed := false
	_, err := readUserJSONL(path, false, func(raw []byte) error {
		if _, err := userRecord(raw); err != nil {
			return err
		}
		if !changed {
			changed = true
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			_, err = file.WriteString(userJSONLRecordB + "\n")
			file.Close()
			return err
		}
		return nil
	})
	if err == nil || !changed {
		t.Fatal("an input changed during its observation without refusal")
	}
}

func TestUserJSONLBoundsRejectStructureBeforeWindowAllocation(t *testing.T) {
	nested := strings.TrimSuffix(userJSONLRecordA, "}") + `,"extra":` + strings.Repeat("[", userJSONLDepthLimit+1) + "0" + strings.Repeat("]", userJSONLDepthLimit+1) + "}"
	if _, err := userRecord([]byte(nested)); err == nil {
		t.Fatal("JSONL nesting exceeded its explicit bound")
	}
	largeTime := fmt.Sprintf(`{"t":%d,"profile":"light","action":"synthetic-read","kind":"read"}`, int64(math.MaxInt64))
	dir := userJSONLFixture(t, userJSONLRecordA+"\n"+largeTime+"\n", "")
	if summary, err := SummarizeUsers(dir); err == nil || summary != nil {
		t.Fatal("implausible report span reached unbounded window allocation")
	}
}

func TestUserJSONLOutOfRangeSamplesAreCountedSeparately(t *testing.T) {
	dir := userJSONLFixture(t, userJSONLRecordA+"\n"+userJSONLRecordB+"\n", userJSONLSampleA+"\n"+`{"t":3700000}`+"\n")
	summary, err := SummarizeUsers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Inputs.Samples.Rows != 2 || summary.Inputs.Samples.UsedRows != 1 {
		t.Fatal("sample framing evidence was confused with selected-window coverage")
	}
}

func TestUserJSONLFinalCompleteInvalidObjectsAreNotSalvaged(t *testing.T) {
	for _, tail := range []string{"null", "{}", userJSONLRecordA + "{}", "synthetic-sensitive-content"} {
		dir := userJSONLFixture(t, userJSONLRecordA+"\n"+tail, userJSONLSampleA+"\n")
		if summary, err := SummarizeUsers(dir); err == nil || summary != nil {
			t.Fatal("complete invalid final data was mislabeled as an interrupted write")
		}
	}
}

func TestUserJSONLDockerDiagnosticsDistinguishUnselectedAndUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name, raw, reason string
		selected, fail    bool
	}{
		{"none-selected", "{", "", false, false},
		{"garbled", "{", "malformed output", true, false},
		{"command-failed", "", "command unavailable", true, true},
		{"missing-observation", "", "incomplete observations", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resourceDocker(t, tc.raw)
			if tc.fail {
				if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "docker"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/api/blocks" {
					io.WriteString(out, "[]")
				} else {
					io.WriteString(out, "{}")
				}
			}))
			defer server.Close()
			names := DPNames()
			e := &Env{Names: names, running: map[string]bool{names.Node: tc.selected}, API: server.URL, http: server.Client()}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			sample := e.sampleOnce(ctx)
			if tc.reason == "" {
				if len(sample.Errors) != 0 {
					t.Fatal("unselected Docker probe was labeled as a failure")
				}
			} else if len(sample.Errors) != 1 || sample.Errors[0] != "docker stats: "+tc.reason {
				t.Fatalf("Docker probe failure lost its concise diagnostic: %v", sample.Errors)
			}
		})
	}
}
