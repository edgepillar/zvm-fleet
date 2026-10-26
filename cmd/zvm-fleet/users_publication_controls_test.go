package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func publicationPair(t *testing.T) (string, map[string][]byte) {
	t.Helper()
	dir := publicationFixture(t)
	if err := writeUsersReport(dir, filepath.Join(dir, "USERS.md")); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, name := range []string{"USERS.md", "summary.json", userReportReceiptName} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = raw
	}
	return dir, before
}

func publicationPlan(t *testing.T, dir, out string) *userReportPublication {
	t.Helper()
	p, err := openUserReportPublication(dir, out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.close(); err != nil {
			t.Error(err)
		}
	})
	return p
}

func publicationSamePair(t *testing.T, dir string, before map[string][]byte) {
	t.Helper()
	for name, want := range before {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(raw, want) {
			t.Errorf("previous pair changed: %s", name)
		}
	}
}

func TestUserReportPublishesAndVerifiesRepeatedPairs(t *testing.T) {
	dir, before := publicationPair(t)
	if err := verifyUsersReport(dir, filepath.Join(dir, "USERS.md")); err != nil {
		t.Fatal(err)
	}
	var receipt userReportReceipt
	if json.Unmarshal(before[userReportReceiptName], &receipt) != nil || receipt.State != "published" || receipt.Markdown != reportDigest(before["USERS.md"]) || receipt.Summary != reportDigest(before["summary.json"]) {
		t.Fatal("receipt did not bind the published pair")
	}
	if err := os.WriteFile(filepath.Join(dir, "records.jsonl"), []byte(writerReportRecord+strings.Replace(writerReportRecord, "1000", "2000", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeUsersReport(dir, filepath.Join(dir, "USERS.md")); err != nil {
		t.Fatal(err)
	}
	if err := verifyUsersReport(dir, filepath.Join(dir, "USERS.md")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, userReportReceiptName))
	if err != nil || bytes.Equal(raw, before[userReportReceiptName]) {
		t.Fatal("refresh did not commit a new receipt")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".report-") || entry.Name() == userReportLockName {
			t.Fatal("successful publication retained owned temporary files or a claim")
		}
	}
}

func TestUserReportCustomOutputAndBasicModes(t *testing.T) {
	dir := publicationFixture(t)
	out := filepath.Join(t.TempDir(), "external.md")
	if err := os.WriteFile(out, []byte("previous external report"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(out, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := writeUsersReport(dir, out); err != nil {
		t.Fatal(err)
	}
	if err := verifyUsersReport(dir, out); err != nil {
		t.Fatal(err)
	}
	publicationRetained(t, filepath.Join(dir, "USERS.md"), "retained synthetic Markdown\n")
	if runtime.GOOS != "windows" {
		info, err := os.Stat(out)
		if err != nil || info.Mode().Perm() != 0o640 {
			t.Fatal("external report permissions changed")
		}
		info, err = os.Stat(filepath.Join(dir, userReportReceiptName))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("new report receipt was not private")
		}
	}
	if err := verifyUsersReport(dir, filepath.Join(dir, "USERS.md")); err == nil {
		t.Fatal("receipt accepted a different Markdown destination")
	}
}

func TestUserReportStagingCollisionPreservesPair(t *testing.T) {
	dir, before := publicationPair(t)
	p := publicationPlan(t, dir, filepath.Join(dir, "USERS.md"))
	name := ".report-" + p.id + "-summary.new"
	if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := p.publish([]byte("new Markdown"), []byte(`{"new":true}`)); err == nil {
		t.Fatal("staging collision was accepted")
	}
	publicationSamePair(t, dir, before)
	if info, err := os.Stat(filepath.Join(dir, name)); err != nil || !info.IsDir() {
		t.Fatal("foreign staging collision was changed")
	}
	if err := verifyUsersReport(dir, filepath.Join(dir, "USERS.md")); err != nil {
		t.Fatal("staging refusal invalidated the previous pair", err)
	}
}

func TestUserReportRollsBackObservedPublicationFailures(t *testing.T) {
	for _, kind := range []string{"summary", "receipt", "ambiguous-summary"} {
		t.Run(kind, func(t *testing.T) {
			dir, before := publicationPair(t)
			p := publicationPlan(t, dir, filepath.Join(dir, "USERS.md"))
			want := errors.New("synthetic publication rename failure")
			p.rename = func(r *os.Root, a, b string) error {
				selected := "-" + kind + ".new"
				if kind == "ambiguous-summary" {
					selected = "-summary.new"
				}
				if strings.HasSuffix(a, selected) {
					if kind == "ambiguous-summary" {
						if err := r.Rename(a, b); err != nil {
							return err
						}
					}
					return want
				}
				return r.Rename(a, b)
			}
			if err := p.publish([]byte("new Markdown"), []byte(`{"new":true}`)); !errors.Is(err, want) || p.unknown {
				t.Fatal("known failed rename was lost or not reconciled", err)
			}
			publicationSamePair(t, dir, before)
			if err := verifyUsersReport(dir, filepath.Join(dir, "USERS.md")); err != nil {
				t.Fatal("rollback did not restore the verified prior pair", err)
			}
		})
	}
}

func TestUserReportUnknownReplacementPreservesRecoveryEvidence(t *testing.T) {
	dir, before := publicationPair(t)
	p := publicationPlan(t, dir, filepath.Join(dir, "USERS.md"))
	const foreign = "retained foreign Markdown\n"
	if err := os.WriteFile(filepath.Join(dir, "foreign-candidate"), []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	want := errors.New("synthetic second rename failure")
	p.rename = func(r *os.Root, a, b string) error {
		if strings.HasSuffix(a, "-summary.new") {
			if err := r.Rename("foreign-candidate", "USERS.md"); err != nil {
				return err
			}
			return want
		}
		return r.Rename(a, b)
	}
	err := p.publish([]byte("new Markdown"), []byte(`{"new":true}`))
	if !errors.Is(err, want) || !p.unknown || !strings.Contains(err.Error(), "UNKNOWN") {
		t.Fatal("unknown replacement was not held", err)
	}
	publicationRetained(t, filepath.Join(dir, "USERS.md"), foreign)
	publicationRetained(t, filepath.Join(dir, userReportReceiptName), string(before[userReportReceiptName]))
	publicationRetained(t, filepath.Join(dir, p.targets[0].backup.name), string(before["USERS.md"]))
	if _, err := os.Lstat(filepath.Join(dir, userReportLockName)); err != nil {
		t.Fatal("unknown publication lost its claim")
	}
	if err := verifyUsersReport(dir, filepath.Join(dir, "USERS.md")); err == nil {
		t.Fatal("unknown publication verified successfully")
	}
}

func TestUserReportParentReplacementCannotRedirectWrites(t *testing.T) {
	dir, before := publicationPair(t)
	p := publicationPlan(t, dir, filepath.Join(dir, "USERS.md"))
	retained := dir + "-retained"
	if err := os.Rename(dir, retained); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(retained) })
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := p.publish([]byte("new Markdown"), []byte(`{"new":true}`)); err == nil {
		t.Fatal("replacement parent accepted publication")
	}
	publicationSamePair(t, retained, before)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("publication wrote into replacement parent")
	}
}

func TestUserReportChangedClaimIsHeld(t *testing.T) {
	dir, _ := publicationPair(t)
	p := publicationPlan(t, dir, filepath.Join(dir, "USERS.md"))
	const changed = "retained changed publisher claim\n"
	p.rename = func(r *os.Root, a, b string) error {
		if strings.HasSuffix(a, "-markdown.new") {
			f, err := r.OpenFile(userReportLockName, os.O_WRONLY|os.O_TRUNC, 0)
			if err != nil {
				return err
			}
			_, writeErr := f.Write([]byte(changed))
			if err := errors.Join(writeErr, f.Close()); err != nil {
				return err
			}
		}
		return r.Rename(a, b)
	}
	err := p.publish([]byte("new Markdown"), []byte(`{"new":true}`))
	if err == nil || !p.unknown || !strings.Contains(err.Error(), "UNKNOWN") {
		t.Fatal("changed publisher claim was not held")
	}
	publicationRetained(t, filepath.Join(dir, userReportLockName), changed)
}

func TestUserReportCooperatingPublishersAreSerialized(t *testing.T) {
	dir, _ := publicationPair(t)
	first := publicationPlan(t, dir, filepath.Join(dir, "USERS.md"))
	second := publicationPlan(t, dir, filepath.Join(dir, "USERS.md"))
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	first.rename = func(r *os.Root, a, b string) error {
		if strings.HasSuffix(a, "-markdown.new") {
			close(entered)
			<-release
		}
		return r.Rename(a, b)
	}
	go func() { done <- first.publish([]byte("first Markdown"), []byte(`{"first":true}`)) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first publisher did not reach commit")
	}
	if err := second.publish([]byte("second Markdown"), []byte(`{"second":true}`)); !errors.Is(err, os.ErrExist) {
		t.Error("second publisher did not refuse the exclusive claim", err)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first publisher did not finish")
	}
	if err := verifyUsersReport(dir, filepath.Join(dir, "USERS.md")); err != nil {
		t.Fatal(err)
	}
	publicationRetained(t, filepath.Join(dir, "USERS.md"), "first Markdown")
}

func TestUserReportVerificationRejectsChangedOrUncommittedPairs(t *testing.T) {
	for _, kind := range []string{"markdown", "summary", "receipt", "claim"} {
		t.Run(kind, func(t *testing.T) {
			dir, _ := publicationPair(t)
			name := map[string]string{"markdown": "USERS.md", "summary": "summary.json", "receipt": userReportReceiptName, "claim": userReportLockName}[kind]
			if err := os.WriteFile(filepath.Join(dir, name), []byte("changed synthetic bytes\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verifyUsersReport(dir, filepath.Join(dir, "USERS.md")); err == nil {
				t.Fatal("changed or uncommitted pair verified")
			}
		})
	}
}

func TestUserReportReservedNamesAreCaseIndependent(t *testing.T) {
	dir := publicationFixture(t)
	for _, name := range []string{"NAMES.JSON", "STATE.JSON", "RECORDS.JSONL", "SUMMARY.JSON"} {
		if err := writeUsersReport(dir, filepath.Join(dir, name)); err == nil {
			t.Error("case variant of reserved output accepted", name)
		}
	}
}

func TestUserReportRollbackFailureIsRetained(t *testing.T) {
	dir, before := publicationPair(t)
	p := publicationPlan(t, dir, filepath.Join(dir, "USERS.md"))
	commitErr, rollbackErr := errors.New("synthetic commit failure"), errors.New("synthetic rollback failure")
	p.rename = func(r *os.Root, a, b string) error {
		if strings.HasSuffix(a, "-summary.new") {
			return commitErr
		}
		if strings.HasSuffix(a, "-markdown.previous") {
			return rollbackErr
		}
		return r.Rename(a, b)
	}
	err := p.publish([]byte("new Markdown"), []byte(`{"new":true}`))
	if !errors.Is(err, commitErr) || !errors.Is(err, rollbackErr) || !p.unknown || !strings.Contains(err.Error(), "UNKNOWN") {
		t.Fatal("rollback failure replaced the commit failure or was not held", err)
	}
	publicationRetained(t, filepath.Join(dir, p.targets[0].backup.name), string(before["USERS.md"]))
	publicationRetained(t, filepath.Join(dir, userReportReceiptName), string(before[userReportReceiptName]))
	if _, err := os.Stat(filepath.Join(dir, userReportLockName)); err != nil {
		t.Fatal("failed rollback lost its publisher claim")
	}
}

func TestUserReportUnobservedRenameCannotCommit(t *testing.T) {
	dir, before := publicationPair(t)
	p := publicationPlan(t, dir, filepath.Join(dir, "USERS.md"))
	p.rename = func(r *os.Root, a, b string) error {
		if strings.HasSuffix(a, "-summary.new") {
			return nil
		}
		return r.Rename(a, b)
	}
	if err := p.publish([]byte("new Markdown"), []byte(`{"new":true}`)); err == nil {
		t.Fatal("a rename without an observed output committed successfully")
	}
	publicationSamePair(t, dir, before)
	if err := verifyUsersReport(dir, filepath.Join(dir, "USERS.md")); err != nil {
		t.Fatal(err)
	}
}

func TestUserReportFirstPublicationFailureRemovesOnlyOwnOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "records.jsonl"), []byte(writerReportRecord), 0o600); err != nil {
		t.Fatal(err)
	}
	p := publicationPlan(t, dir, filepath.Join(dir, "USERS.md"))
	want := errors.New("synthetic first publication failure")
	p.rename = func(r *os.Root, a, b string) error {
		if strings.HasSuffix(a, "-summary.new") {
			return want
		}
		return r.Rename(a, b)
	}
	if err := p.publish([]byte("new Markdown"), []byte(`{"new":true}`)); !errors.Is(err, want) || p.unknown {
		t.Fatal("first publication failure was not reconciled", err)
	}
	for _, name := range []string{"USERS.md", "summary.json", userReportReceiptName, userReportLockName} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Error("failed first publication left an output or claim", name)
		}
	}
	publicationRetained(t, filepath.Join(dir, "records.jsonl"), writerReportRecord)
}

func TestUserReportCLIVerifiesStoredPairWithoutClaimingSourceFreshness(t *testing.T) {
	dir, before := publicationPair(t)
	if err := os.WriteFile(filepath.Join(dir, "records.jsonl"), []byte("invalid later source row\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := benchUsersReport([]string{"-in", dir, "-verify"}); err != nil {
		t.Fatal("stored-pair verification rebuilt unrelated later input", err)
	}
	if err := writeUsersReport(dir, filepath.Join(dir, "USERS.md")); err == nil {
		t.Fatal("invalid current source could rebuild a report")
	}
	publicationSamePair(t, dir, before)
}
