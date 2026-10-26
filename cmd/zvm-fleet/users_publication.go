package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const userReportReceiptName = "users-report.json"
const userReportLockName = "users-report.lock"

type userReportDigest struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type userReportReceipt struct {
	Schema     int              `json:"schema"`
	State      string           `json:"state"`
	Generation string           `json:"generation"`
	Markdown   userReportDigest `json:"markdown"`
	Summary    userReportDigest `json:"summary"`
}

type reportDirectory struct {
	root *os.Root
	path string
	info os.FileInfo
}

func openReportDirectory(path string) (*reportDirectory, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	info, err := r.Stat(".")
	if err != nil {
		return nil, errors.Join(err, r.Close())
	}
	d := &reportDirectory{r, abs, info}
	if err := d.check(); err != nil {
		return nil, errors.Join(err, r.Close())
	}
	return d, nil
}

func (d *reportDirectory) check() error {
	current, err := os.Stat(d.path)
	opened, openErr := d.root.Stat(".")
	if err != nil || openErr != nil || !current.IsDir() || !os.SameFile(current, d.info) || !os.SameFile(opened, d.info) {
		return errors.New("report parent directory changed")
	}
	return nil
}

func sameReportFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && a.Mode().IsRegular() && b.Mode().IsRegular() && os.SameFile(a, b) &&
		a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func reportDigest(raw []byte) userReportDigest {
	sum := sha256.Sum256(raw)
	return userReportDigest{hex.EncodeToString(sum[:]), int64(len(raw))}
}

// Hash through the opened file and check its name again. This observes stable
// bytes/identity, without authenticating another process or taking a snapshot.
func hashReportFile(d *reportDirectory, name string, info os.FileInfo) (digest userReportDigest, result error) {
	if err := d.check(); err != nil {
		return digest, err
	}
	f, err := d.root.Open(name)
	if err != nil {
		return digest, err
	}
	defer func() { result = errors.Join(result, f.Close()) }()
	opened, err := f.Stat()
	if err != nil || !sameReportFile(info, opened) {
		return digest, errors.New("report file changed before reading")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, info.Size()+1))
	after, statErr := f.Stat()
	current, pathErr := d.root.Lstat(name)
	if err != nil || statErr != nil || pathErr != nil || n != info.Size() || !sameReportFile(info, after) || !sameReportFile(info, current) {
		return digest, errors.Join(err, errors.New("report file changed while reading"))
	}
	return userReportDigest{hex.EncodeToString(h.Sum(nil)), n}, d.check()
}

type reportStage struct {
	name   string
	info   os.FileInfo
	digest userReportDigest
}

type reportTarget struct {
	dir           *reportDirectory
	name, role    string
	before        os.FileInfo
	original      *os.File
	beforeDigest  userReportDigest
	data          []byte
	stage, backup *reportStage
	published     bool
}

func (t *reportTarget) pathError(err error) error {
	return &os.PathError{Op: "users-report", Path: filepath.Join(t.dir.path, t.name), Err: err}
}

func (t *reportTarget) checkBefore() error {
	if err := t.dir.check(); err != nil {
		return err
	}
	current, err := t.dir.root.Lstat(t.name)
	if t.before == nil && os.IsNotExist(err) {
		return nil
	}
	if err != nil || !sameReportFile(t.before, current) {
		return t.pathError(errors.New("report destination changed"))
	}
	digest, err := hashReportFile(t.dir, t.name, current)
	if err != nil || digest != t.beforeDigest {
		return t.pathError(errors.Join(err, errors.New("report destination bytes changed")))
	}
	return nil
}

func (d *reportDirectory) checkStage(name string, stage *reportStage) error {
	if err := d.check(); err != nil {
		return err
	}
	current, err := d.root.Lstat(name)
	if err != nil || !sameReportFile(stage.info, current) {
		return errors.New("owned report file changed")
	}
	digest, err := hashReportFile(d, name, current)
	if err != nil || digest != stage.digest {
		return errors.Join(err, errors.New("owned report bytes changed"))
	}
	return nil
}

func (d *reportDirectory) create(name string, reader io.Reader, want userReportDigest, mode os.FileMode) (stage *reportStage, result error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	f, err := d.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, f.Close()) }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), reader)
	if err != nil || n != want.Bytes || hex.EncodeToString(h.Sum(nil)) != want.SHA256 {
		return nil, errors.Join(err, errors.New("incomplete report staging; new file retained for inspection"))
	}
	if err := f.Chmod(mode.Perm()); err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	stage = &reportStage{name, info, want}
	return stage, d.checkStage(name, stage)
}

type userReportPublication struct {
	dirs    []*reportDirectory
	targets []*reportTarget
	id      string
	claim   *reportStage
	unknown bool
	// A per-publication operation permits deterministic filesystem failure
	// controls without changing global functions or concurrent publishers.
	rename func(*os.Root, string, string) error
}

func (p *userReportPublication) close() error {
	var result error
	for _, t := range p.targets {
		if t.original != nil {
			result = errors.Join(result, t.original.Close())
			t.original = nil
		}
	}
	for _, d := range p.dirs {
		result = errors.Join(result, d.root.Close())
	}
	p.dirs = nil
	return result
}

func reservedUserReportInput(name string) bool {
	name = strings.ToLower(name)
	return name == "records.jsonl" || name == "samples.jsonl" || name == userReportLockName || strings.HasSuffix(name, ".json")
}

func openUserReportPublication(in, out string) (result *userReportPublication, failure error) {
	p := &userReportPublication{rename: func(r *os.Root, a, b string) error { return r.Rename(a, b) }}
	defer func() {
		if failure != nil {
			failure = errors.Join(failure, p.close())
		}
	}()
	d, err := openReportDirectory(in)
	if err != nil {
		return nil, err
	}
	p.dirs = append(p.dirs, d)
	abs, err := filepath.Abs(out)
	if err != nil {
		return nil, err
	}
	md, err := openReportDirectory(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	p.dirs = append(p.dirs, md)
	name := filepath.Base(abs)
	if reservedUserReportInput(name) {
		return nil, &os.PathError{Op: "users-report", Path: out, Err: errors.New("Markdown cannot replace retained input or a reserved report file")}
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	p.id = hex.EncodeToString(token[:])
	p.targets = []*reportTarget{{dir: md, name: name, role: "markdown"}, {dir: d, name: "summary.json", role: "summary"}, {dir: d, name: userReportReceiptName, role: "receipt"}}
	var protected []os.FileInfo
	folder, err := d.root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, readErr := folder.ReadDir(-1)
	if err := errors.Join(readErr, folder.Close()); err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !reservedUserReportInput(entry.Name()) || entry.Name() == "summary.json" || entry.Name() == userReportReceiptName || entry.Name() == userReportLockName {
			continue
		}
		info, err := d.root.Lstat(entry.Name())
		if err != nil {
			return nil, err
		}
		protected = append(protected, info)
	}
	for i, t := range p.targets {
		info, err := t.dir.root.Lstat(t.name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, t.pathError(err)
		}
		if !info.Mode().IsRegular() {
			return nil, t.pathError(errors.New("report destination must be a regular file"))
		}
		for _, before := range protected {
			if os.SameFile(info, before) {
				return nil, t.pathError(errors.New("report destination aliases retained input"))
			}
		}
		for _, other := range p.targets[:i] {
			if other.before != nil && os.SameFile(info, other.before) {
				return nil, t.pathError(errors.New("report destinations share a file identity"))
			}
		}
		t.before = info
		t.original, err = t.dir.root.Open(t.name)
		if err != nil {
			return nil, t.pathError(err)
		}
		opened, err := t.original.Stat()
		if err != nil || !sameReportFile(info, opened) {
			return nil, t.pathError(errors.New("report destination changed while opening"))
		}
		t.beforeDigest, err = hashReportFile(t.dir, t.name, info)
		if err != nil {
			return nil, t.pathError(err)
		}
	}
	if p.targets[2].before != nil {
		if _, err := p.receipt(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *userReportPublication) receipt() (*userReportReceipt, error) {
	t := p.targets[2]
	if t.before == nil || t.before.Size() > 4096 {
		return nil, errors.New("verified user report receipt is unavailable")
	}
	raw, err := io.ReadAll(io.NewSectionReader(t.original, 0, t.before.Size()+1))
	if err != nil {
		return nil, err
	}
	var receipt userReportReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return nil, errors.New("invalid user report receipt")
	}
	canonical, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil || !bytes.Equal(raw, append(canonical, '\n')) || receipt.Schema != 1 || receipt.State != "published" || len(receipt.Generation) != 32 {
		return nil, errors.New("invalid or noncanonical user report receipt")
	}
	if id, err := hex.DecodeString(receipt.Generation); err != nil || len(id) != 16 || strings.ToLower(receipt.Generation) != receipt.Generation {
		return nil, errors.New("invalid user report generation")
	}
	if p.targets[0].before == nil || p.targets[1].before == nil || receipt.Markdown != p.targets[0].beforeDigest || receipt.Summary != p.targets[1].beforeDigest {
		return nil, errors.New("user report pair does not match its receipt")
	}
	return &receipt, nil
}

func (p *userReportPublication) claimCheck() error {
	if p.claim == nil {
		return errors.New("user report publisher has no claim")
	}
	return p.dirs[0].checkStage(userReportLockName, p.claim)
}

func (p *userReportPublication) removeOwned(d *reportDirectory, name string, stage *reportStage) error {
	if err := d.checkStage(name, stage); err != nil {
		return err
	}
	return d.root.Remove(name)
}

func (p *userReportPublication) publish(markdown, summary []byte) (result error) {
	d := p.dirs[0]
	claimRaw, err := json.Marshal(struct {
		Schema     int    `json:"schema"`
		Generation string `json:"generation"`
	}{1, p.id})
	if err != nil {
		return err
	}
	p.claim, err = d.create(userReportLockName, bytes.NewReader(claimRaw), reportDigest(claimRaw), 0o600)
	if err != nil {
		return fmt.Errorf("claiming user report publisher: %w", err)
	}
	defer func() {
		if !p.unknown {
			if err := p.removeOwned(d, userReportLockName, p.claim); err != nil {
				p.unknown = true
				result = errors.Join(result, err)
			}
		}
		if p.unknown {
			result = errors.Join(result, errors.New("report publication is UNKNOWN; claim and recovery files retained for inspection"))
		}
	}()
	for _, t := range p.targets {
		if err := t.checkBefore(); err != nil {
			return err
		}
	}
	receipt := userReportReceipt{1, "published", p.id, reportDigest(markdown), reportDigest(summary)}
	raw, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	data := [][]byte{markdown, summary, append(raw, '\n')}
	for i, t := range p.targets {
		t.data = data[i]
		mode := os.FileMode(0o600)
		if t.before != nil {
			mode = t.before.Mode().Perm()
		}
		t.stage, err = t.dir.create(".report-"+p.id+"-"+t.role+".new", bytes.NewReader(t.data), reportDigest(t.data), mode)
		if err != nil {
			return fmt.Errorf("staging %s: %w", t.role, err)
		}
		if t.before != nil {
			t.backup, err = t.dir.create(".report-"+p.id+"-"+t.role+".previous", io.NewSectionReader(t.original, 0, t.before.Size()+1), t.beforeDigest, t.before.Mode().Perm())
			if err != nil {
				return fmt.Errorf("retaining previous %s: %w", t.role, err)
			}
		}
	}
	for _, t := range p.targets {
		if err := p.claimCheck(); err != nil {
			p.unknown = true
			return errors.Join(err, p.rollback())
		}
		for _, other := range p.targets {
			var check error
			if other.published {
				check = other.dir.checkStage(other.name, other.stage)
			} else {
				check = other.checkBefore()
			}
			if check != nil {
				return errors.Join(check, p.rollback())
			}
		}
		if err := t.dir.checkStage(t.stage.name, t.stage); err != nil {
			return errors.Join(err, p.rollback())
		}
		renameErr := p.rename(t.dir.root, t.stage.name, t.name)
		observed := t.dir.checkStage(t.name, t.stage)
		if observed == nil {
			t.published = true
		} else if t.checkBefore() != nil || t.dir.checkStage(t.stage.name, t.stage) != nil {
			p.unknown = true
		}
		if renameErr != nil || observed != nil {
			return errors.Join(renameErr, observed, p.rollback())
		}
	}
	for _, t := range p.targets {
		if err := t.dir.checkStage(t.name, t.stage); err != nil {
			return errors.Join(err, p.rollback())
		}
	}
	for _, t := range p.targets {
		if t.backup != nil {
			if err := p.removeOwned(t.dir, t.backup.name, t.backup); err != nil {
				p.unknown = true
				return err
			}
		}
		if _, err := t.dir.root.Lstat(t.stage.name); !os.IsNotExist(err) {
			p.unknown = true
			return errors.New("report staging name replaced; retained for inspection")
		}
	}
	return nil
}

// Two file renames are not a transaction. Roll back only observed owned
// replacements; uncertainty holds the claim and all remaining recovery files.
func (p *userReportPublication) rollback() error {
	var result error
	for i := len(p.targets) - 1; i >= 0; i-- {
		t := p.targets[i]
		if !t.published {
			continue
		}
		if err := errors.Join(p.claimCheck(), t.dir.checkStage(t.name, t.stage)); err != nil {
			p.unknown = true
			result = errors.Join(result, err)
			continue
		}
		var err error
		if t.backup == nil {
			err = t.dir.root.Remove(t.name)
		} else {
			if err = t.dir.checkStage(t.backup.name, t.backup); err == nil {
				err = p.rename(t.dir.root, t.backup.name, t.name)
				t.before = t.backup.info
			}
		}
		check := t.checkBefore()
		if check != nil {
			p.unknown = true
		} else {
			t.published = false
		}
		result = errors.Join(result, err, check)
	}
	return result
}

func publishUsersReport(in, out string, markdown, summary []byte) (result error) {
	p, err := openUserReportPublication(in, out)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, p.close()) }()
	return p.publish(markdown, summary)
}

func verifyUsersReport(in, out string) (result error) {
	p, err := openUserReportPublication(in, out)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, p.close()) }()
	noClaim := func() error {
		if _, err := p.dirs[0].root.Lstat(userReportLockName); !os.IsNotExist(err) {
			return errors.New("user report publication is in progress or requires inspection")
		}
		return nil
	}
	if err := noClaim(); err != nil {
		return err
	}
	if _, err := p.receipt(); err != nil {
		return err
	}
	for _, t := range p.targets {
		if err := t.checkBefore(); err != nil {
			return err
		}
	}
	return noClaim()
}
