//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func consumerPair(t *testing.T, markdown, summary []byte) (string, string) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "USERS.md")
	if err := publishUsersReport(dir, out, markdown, summary); err != nil {
		t.Fatal(err)
	}
	return dir, out
}

func consumerChange(t *testing.T, dir string, change func(*userReportReceipt)) {
	t.Helper()
	name := filepath.Join(dir, userReportReceiptName)
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var receipt userReportReceipt
	if json.Unmarshal(raw, &receipt) != nil {
		t.Fatal("reading owned receipt fixture failed")
	}
	change(&receipt)
	raw, err = json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUserReportConsumerReadsPublishedBytes(t *testing.T) {
	for _, kind := range []string{"markdown", "summary", "external-markdown", "empty-content"} {
		t.Run(kind, func(t *testing.T) {
			markdown, summary := []byte("owned Markdown\n"), []byte("{\"owned\":true}\n")
			if kind == "empty-content" {
				markdown, summary = nil, nil
			}
			dir, out := consumerPair(t, markdown, summary)
			if kind == "external-markdown" {
				out = filepath.Join(t.TempDir(), "external.md")
				if err := os.Rename(filepath.Join(dir, "USERS.md"), out); err != nil {
					t.Fatal(err)
				}
			}
			got, err := readUsersReport(dir, out)
			if err != nil || got == nil || len(got.generation) != 32 || !bytes.Equal(got.markdown, markdown) || !bytes.Equal(got.summary, summary) {
				t.Fatal("consumer did not capture the exact published generation", err)
			}
			member, want := "markdown", markdown
			if kind == "summary" {
				member, want = "summary", summary
			}
			var output bytes.Buffer
			if err := got.write(&output, member); err != nil || !bytes.Equal(output.Bytes(), want) {
				t.Fatal("consumer changed emitted bytes", err)
			}
		})
	}
}

func TestUserReportConsumerRefusesInvalidPublishedPairs(t *testing.T) {
	for _, kind := range []string{"missing-receipt", "missing-summary", "missing-markdown", "changed-markdown", "changed-summary",
		"receipt-state", "receipt-generation", "receipt-digest", "noncanonical-receipt", "publisher-claim"} {
		t.Run(kind, func(t *testing.T) {
			dir, out := consumerPair(t, []byte("owned Markdown\n"), []byte("{\"owned\":true}\n"))
			var err error
			switch kind {
			case "missing-receipt":
				err = os.Rename(filepath.Join(dir, userReportReceiptName), filepath.Join(dir, "retained-receipt"))
			case "missing-summary":
				err = os.Rename(filepath.Join(dir, "summary.json"), filepath.Join(dir, "retained-summary"))
			case "missing-markdown":
				err = os.Rename(out, filepath.Join(dir, "retained-markdown"))
			case "changed-markdown":
				err = os.WriteFile(out, []byte("different Markdown\n"), 0o600)
			case "changed-summary":
				err = os.WriteFile(filepath.Join(dir, "summary.json"), []byte("{\"different\":true}\n"), 0o600)
			case "receipt-state":
				consumerChange(t, dir, func(r *userReportReceipt) { r.State = "pending" })
			case "receipt-generation":
				consumerChange(t, dir, func(r *userReportReceipt) { r.Generation = strings.Repeat("A", 32) })
			case "receipt-digest":
				consumerChange(t, dir, func(r *userReportReceipt) { r.Summary.SHA256 = strings.Repeat("f", 64) })
			case "noncanonical-receipt":
				raw, e := os.ReadFile(filepath.Join(dir, userReportReceiptName))
				if e != nil {
					t.Fatal(e)
				}
				err = os.WriteFile(filepath.Join(dir, userReportReceiptName), append([]byte(" "), raw...), 0o600)
			case "publisher-claim":
				err = os.WriteFile(filepath.Join(dir, userReportLockName), []byte("retained owned claim\n"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, err := readUsersReport(dir, out); err == nil || got != nil {
				t.Fatal("consumer accepted invalid or uncommitted report bytes")
			}
		})
	}
}

func TestUserReportConsumerByteBudgets(t *testing.T) {
	for _, kind := range []string{"exact", "markdown-over", "summary-over", "receipt-over", "declared-over", "invalid-limit"} {
		t.Run(kind, func(t *testing.T) {
			dir, out := consumerPair(t, []byte("12345"), []byte("123456"))
			limits := userReportReadLimits{5, 6}
			var err error
			switch kind {
			case "markdown-over":
				err = os.WriteFile(out, []byte("123456"), 0o600)
			case "summary-over":
				err = os.WriteFile(filepath.Join(dir, "summary.json"), []byte("1234567"), 0o600)
			case "receipt-over":
				err = os.WriteFile(filepath.Join(dir, userReportReceiptName), bytes.Repeat([]byte("x"), int(userReportReadReceiptLimit)+1), 0o600)
			case "declared-over":
				consumerChange(t, dir, func(r *userReportReceipt) { r.Summary.Bytes = 7 })
			case "invalid-limit":
				limits.markdown = userReportReadMarkdownLimit + 1
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := readUsersReportWith(dir, out, limits, openUserReportConsumerFile)
			if kind == "exact" {
				if err != nil || got == nil || string(got.markdown) != "12345" || string(got.summary) != "123456" {
					t.Fatal("exact read boundary failed", err)
				}
			} else if err == nil || got != nil {
				t.Fatal("consumer accepted an exceeded read budget")
			}
		})
	}
}

func TestUserReportConsumerRejectsNameAndFileChanges(t *testing.T) {
	for _, kind := range []string{"reserved-name", "symlink-member", "shared-identity", "changed-already-read", "claim-during-read", "changed-parent"} {
		t.Run(kind, func(t *testing.T) {
			dir, out := consumerPair(t, []byte("same"), []byte("same"))
			open := openUserReportConsumerFile
			switch kind {
			case "reserved-name":
				out = filepath.Join(dir, "STATE.JSON")
			case "symlink-member":
				if err := os.Rename(out, filepath.Join(dir, "retained-markdown")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("retained-markdown", out); err != nil {
					t.Fatal(err)
				}
			case "shared-identity":
				if err := os.Rename(out, filepath.Join(dir, "retained-markdown")); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(filepath.Join(dir, "summary.json"), out); err != nil {
					t.Fatal(err)
				}
			default:
				open = func(root *os.Root, name string) (*os.File, error) {
					if name == "USERS.md" {
						switch kind {
						case "changed-already-read":
							if err := os.WriteFile(filepath.Join(dir, "summary.json"), []byte("changed already captured member"), 0o600); err != nil {
								return nil, err
							}
						case "claim-during-read":
							if err := os.WriteFile(filepath.Join(dir, userReportLockName), []byte("retained owned claim\n"), 0o600); err != nil {
								return nil, err
							}
						case "changed-parent":
							retained := dir + "-retained"
							if err := os.Rename(dir, retained); err != nil {
								return nil, err
							}
							t.Cleanup(func() { os.RemoveAll(retained) })
							if err := os.Mkdir(dir, 0o700); err != nil {
								return nil, err
							}
						}
					}
					return openUserReportConsumerFile(root, name)
				}
			}
			got, err := readUsersReportWith(dir, out, userReportReadLimits{1024, 1024}, open)
			if err == nil || got != nil {
				t.Fatal("consumer accepted a reserved, aliased or changed file/claim identity")
			}
		})
	}
}

func TestUserReportConsumerRetainsCapturedGeneration(t *testing.T) {
	dir, out := consumerPair(t, []byte("first Markdown"), []byte("first summary"))
	old, err := readUsersReport(dir, out)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishUsersReport(dir, out, []byte("second Markdown"), []byte("second summary")); err != nil {
		t.Fatal(err)
	}
	latest, err := readUsersReport(dir, out)
	if err != nil {
		t.Fatal(err)
	}
	if old.generation == latest.generation {
		t.Fatal("fixture did not publish a different generation")
	}
	for _, member := range []string{"markdown", "summary"} {
		var output bytes.Buffer
		if err := old.write(&output, member); err != nil {
			t.Fatal(err)
		}
		if output.String() != "first "+map[string]string{"markdown": "Markdown", "summary": "summary"}[member] {
			t.Fatal("emission reopened the newer report paths")
		}
	}
}

type consumerWriter struct {
	n     int
	err   error
	calls int
}

func (w *consumerWriter) Write(raw []byte) (int, error) { w.calls++; return w.n, w.err }

func TestUserReportConsumerOutputErrors(t *testing.T) {
	want := errors.New("owned output refused")
	report := &checkedUserReport{summary: []byte("captured")}
	for _, kind := range []string{"short", "failed", "invalid-count", "invalid-member"} {
		t.Run(kind, func(t *testing.T) {
			writer := &consumerWriter{n: 8}
			member := "summary"
			switch kind {
			case "short":
				writer.n = 3
			case "failed":
				writer.err = want
			case "invalid-count":
				writer.n = 9
			case "invalid-member":
				member = "other"
			}
			err := report.write(writer, member)
			if err == nil {
				t.Fatal("consumer reported successful incomplete/invalid output")
			}
			if kind == "failed" && !errors.Is(err, want) {
				t.Fatal("original output failure was lost")
			}
			if (kind == "short" || kind == "invalid-count") && !errors.Is(err, io.ErrShortWrite) {
				t.Fatal("invalid output count was lost")
			}
			if kind == "invalid-member" && writer.calls != 0 || writer.calls > 1 {
				t.Fatal("consumer retried output or emitted an invalid selection")
			}
		})
	}
}

func TestUserReportConsumerCLIMode(t *testing.T) {
	for _, kind := range []string{"markdown", "summary", "verify-and-read", "invalid-member", "empty-member", "extra-argument"} {
		t.Run(kind, func(t *testing.T) {
			dir, before := publicationPair(t)
			if err := os.WriteFile(filepath.Join(dir, "records.jsonl"), []byte("invalid later raw input\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			member := "summary"
			if kind == "markdown" {
				member = "markdown"
			}
			if kind == "invalid-member" {
				member = "other"
			}
			if kind == "empty-member" {
				member = ""
			}
			args := []string{"-in", dir, "-read", member}
			if kind == "verify-and-read" {
				args = append(args, "-verify")
			}
			if kind == "extra-argument" {
				args = append(args, "unexpected")
			}
			var output bytes.Buffer
			err := benchUsersReportTo(args, &output)
			if kind == "invalid-member" || kind == "empty-member" || kind == "extra-argument" {
				if err == nil || output.Len() != 0 {
					t.Fatal("invalid read mode rebuilt or emitted a report")
				}
			} else {
				name := "summary.json"
				if member == "markdown" {
					name = "USERS.md"
				}
				if err != nil || !bytes.Equal(output.Bytes(), before[name]) {
					t.Fatal("CLI did not emit the exact verified stored member", err)
				}
			}
			publicationSamePair(t, dir, before)
			publicationRetained(t, filepath.Join(dir, "records.jsonl"), "invalid later raw input\n")
		})
	}
}

func TestUserReportConsumerRefusesCooperatingPublisher(t *testing.T) {
	dir, _ := publicationPair(t)
	out := filepath.Join(dir, "USERS.md")
	publisher := publicationPlan(t, dir, out)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	publisher.rename = func(root *os.Root, from, to string) error {
		if strings.HasSuffix(from, "-markdown.new") {
			close(entered)
			<-release
		}
		return root.Rename(from, to)
	}
	go func() { done <- publisher.publish([]byte("new owned Markdown"), []byte("new owned summary")) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("owned publisher did not reach its claim")
	}
	if got, err := readUsersReport(dir, out); err == nil || got != nil {
		t.Error("consumer accepted an active publisher")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owned publisher did not finish")
	}
	if got, err := readUsersReport(dir, out); err != nil || got == nil {
		t.Fatal("finished publication remained unreadable", err)
	}
}
