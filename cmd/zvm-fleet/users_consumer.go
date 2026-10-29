package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const userReportReadMarkdownLimit int64 = 16 << 20
const userReportReadSummaryLimit int64 = 32 << 20
const userReportReadReceiptLimit int64 = 4096

type userReportReadLimits struct{ markdown, summary int64 }

func decodeUserReportReceipt(raw []byte) (*userReportReceipt, error) {
	if len(raw) > int(userReportReadReceiptLimit) {
		return nil, errors.New("user report receipt exceeds the read limit")
	}
	var receipt userReportReceipt
	if json.Unmarshal(raw, &receipt) != nil {
		return nil, errors.New("invalid user report receipt")
	}
	canonical, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil || !bytes.Equal(raw, append(canonical, '\n')) || receipt.Schema != 1 || receipt.State != "published" {
		return nil, errors.New("invalid or noncanonical user report receipt")
	}
	generation, err := hex.DecodeString(receipt.Generation)
	if err != nil || len(receipt.Generation) != 32 || len(generation) != 16 || strings.ToLower(receipt.Generation) != receipt.Generation {
		return nil, errors.New("invalid user report generation")
	}
	for _, digest := range []userReportDigest{receipt.Markdown, receipt.Summary} {
		sum, err := hex.DecodeString(digest.SHA256)
		if err != nil || len(digest.SHA256) != 64 || len(sum) != 32 || strings.ToLower(digest.SHA256) != digest.SHA256 || digest.Bytes < 0 {
			return nil, errors.New("invalid user report digest")
		}
	}
	return &receipt, nil
}

type userReportReadMember struct {
	dir        *reportDirectory
	name, role string
	info       os.FileInfo
	raw        []byte
}

type userReportOpen func(*os.Root, string) (*os.File, error)

func (m *userReportReadMember) capture(limit int64, open userReportOpen) (result error) {
	if err := m.dir.check(); err != nil {
		return err
	}
	before, err := m.dir.root.Lstat(m.name)
	if err != nil {
		return fmt.Errorf("reading user report %s: %w", m.role, err)
	}
	if !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > limit {
		return fmt.Errorf("user report %s must be a regular file within its read limit", m.role)
	}
	f, err := open(m.dir.root, m.name)
	if err != nil {
		return fmt.Errorf("opening user report %s: %w", m.role, err)
	}
	defer func() { result = errors.Join(result, f.Close()) }()
	opened, err := f.Stat()
	if err != nil || !sameReportFile(before, opened) {
		return errors.New("user report member changed before reading")
	}
	raw, err := io.ReadAll(io.LimitReader(f, before.Size()+1))
	after, statErr := f.Stat()
	current, pathErr := m.dir.root.Lstat(m.name)
	if err != nil || statErr != nil || pathErr != nil || int64(len(raw)) != before.Size() ||
		!sameReportFile(before, after) || !sameReportFile(before, current) {
		return errors.Join(err, statErr, pathErr, errors.New("user report member changed while reading"))
	}
	if err := m.dir.check(); err != nil {
		return err
	}
	m.info, m.raw = before, raw
	return nil
}

// The bytes belong to one checked generation. Emission never reopens a path.
// This does not authenticate a writer, establish source freshness or make stdout
// transactional; a consumer must still check the command's exit status.
type checkedUserReport struct {
	generation        string
	markdown, summary []byte
}

func (r *checkedUserReport) write(w io.Writer, member string) error {
	if r == nil || w == nil {
		return errors.New("checked user report output is unavailable")
	}
	var raw []byte
	switch member {
	case "markdown":
		raw = r.markdown
	case "summary":
		raw = r.summary
	default:
		return errors.New("-read must select markdown or summary")
	}
	n, err := w.Write(raw)
	if n != len(raw) {
		err = errors.Join(err, io.ErrShortWrite)
	}
	return err
}

func readUsersReport(in, out string) (*checkedUserReport, error) {
	return readUsersReportWith(in, out, userReportReadLimits{userReportReadMarkdownLimit, userReportReadSummaryLimit}, openUserReportConsumerFile)
}

// A per-call opener permits deterministic file/claim changes in the owned
// filesystem controls without mutating global functions or other readers.
func readUsersReportWith(in, out string, limits userReportReadLimits, open userReportOpen) (snapshot *checkedUserReport, result error) {
	if limits.markdown < 0 || limits.markdown > userReportReadMarkdownLimit || limits.summary < 0 || limits.summary > userReportReadSummaryLimit || open == nil {
		return nil, errors.New("invalid user report read limits or opener")
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(abs)
	if reservedUserReportInput(name) {
		return nil, errors.New("Markdown cannot select a retained input or reserved report file")
	}
	d, err := openReportDirectory(in)
	if err != nil {
		return nil, err
	}
	dirs := []*reportDirectory{d}
	defer func() {
		for _, dir := range dirs {
			result = errors.Join(result, dir.root.Close())
		}
		if result != nil {
			snapshot = nil
		}
	}()
	md, err := openReportDirectory(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	dirs = append(dirs, md)
	noClaim := func() error {
		if _, err := d.root.Lstat(userReportLockName); !os.IsNotExist(err) {
			return errors.New("user report publication is in progress or requires inspection")
		}
		return nil
	}
	if err := noClaim(); err != nil {
		return nil, err
	}
	receipt := &userReportReadMember{dir: d, name: userReportReceiptName, role: "receipt"}
	if err := receipt.capture(userReportReadReceiptLimit, open); err != nil {
		return nil, err
	}
	selected, err := decodeUserReportReceipt(receipt.raw)
	if err != nil {
		return nil, err
	}
	if selected.Markdown.Bytes > limits.markdown || selected.Summary.Bytes > limits.summary {
		return nil, errors.New("user report receipt exceeds the selected payload read limits")
	}
	summary := &userReportReadMember{dir: d, name: "summary.json", role: "summary"}
	markdown := &userReportReadMember{dir: md, name: name, role: "markdown"}
	members := []*userReportReadMember{receipt, summary, markdown}
	for _, member := range members[1:] {
		if err := noClaim(); err != nil {
			return nil, err
		}
		limit := limits.summary
		if member == markdown {
			limit = limits.markdown
		}
		if err := member.capture(limit, open); err != nil {
			return nil, err
		}
	}
	if reportDigest(markdown.raw) != selected.Markdown || reportDigest(summary.raw) != selected.Summary {
		return nil, errors.New("user report captured pair does not match its receipt")
	}
	for i, member := range members {
		if err := member.dir.check(); err != nil {
			return nil, err
		}
		current, err := member.dir.root.Lstat(member.name)
		if err != nil || !sameReportFile(member.info, current) {
			return nil, errors.New("user report member changed before capture completed")
		}
		for _, earlier := range members[:i] {
			if os.SameFile(earlier.info, member.info) {
				return nil, errors.New("user report members share a file identity")
			}
		}
	}
	if err := noClaim(); err != nil {
		return nil, err
	}
	return &checkedUserReport{selected.Generation, markdown.raw, summary.raw}, nil
}
