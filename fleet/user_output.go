package fleet

import (
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

func userOutputName(name string) bool {
	return filepath.IsLocal(name) && filepath.Base(name) == name && !strings.ContainsAny(name, "/\\")
}

// Each operation owns its root descriptor. An output-path rename cannot redirect
// an already opened root or stream into a replacement directory.
func (e *Env) openUserRoot() (*os.Root, error) {
	if e.userRun == nil {
		return nil, errors.New("user output must be claimed with StartUserRun before Init")
	}
	r, err := os.OpenRoot(e.Out)
	if err != nil {
		return nil, errors.New("claimed user output directory is unavailable")
	}
	if err := e.checkUserRoot(r); err != nil {
		return nil, errors.Join(err, r.Close())
	}
	return r, nil
}

func sameUserOutput(a, b os.FileInfo) bool {
	return a.Mode().IsRegular() && b.Mode().IsRegular() && os.SameFile(a, b) &&
		a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func (e *Env) checkUserRoot(r *os.Root) error {
	dir, err := os.Lstat(e.Out)
	opened, rootErr := r.Stat(".")
	if err != nil || rootErr != nil || !dir.IsDir() || !os.SameFile(dir, e.userRun.dir) || !os.SameFile(opened, e.userRun.dir) {
		return errors.New("claimed user output directory changed")
	}
	before, err := r.Lstat(userRunFile)
	if err != nil || !before.Mode().IsRegular() || !os.SameFile(before, e.userRun.marker) || before.Size() > 1024 {
		return errors.New("user run marker changed")
	}
	f, err := r.Open(userRunFile)
	if err != nil {
		return errors.New("user run marker is unavailable")
	}
	fileBefore, statErr := f.Stat()
	if statErr != nil || !sameUserOutput(before, fileBefore) {
		return errors.Join(errors.New("user run marker changed"), f.Close())
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, 1025))
	fileAfter, statErr := f.Stat()
	closeErr := f.Close()
	after, pathErr := r.Lstat(userRunFile)
	if readErr != nil || statErr != nil || closeErr != nil || pathErr != nil ||
		!sameUserOutput(before, fileAfter) || !sameUserOutput(before, after) ||
		int64(len(raw)) != before.Size() || sha256.Sum256(raw) != e.userRun.markerSHA256 {
		return errors.New("user run marker changed")
	}
	dir, err = os.Lstat(e.Out)
	if err != nil || !dir.IsDir() || !os.SameFile(dir, e.userRun.dir) {
		return errors.New("claimed user output directory changed")
	}
	return nil
}

type userOutputFile struct {
	*os.File
	root    *os.Root
	env     *Env
	name    string
	info    os.FileInfo
	mu      sync.Mutex
	failure error
	closed  bool
}

// These are observed identity/metadata/offset checks, not a filesystem snapshot
// or authentication against a noncooperating writer restoring old metadata.
func (f *userOutputFile) check() error {
	if err := f.env.checkUserRoot(f.root); err != nil {
		return err
	}
	opened, statErr := f.File.Stat()
	current, pathErr := f.root.Lstat(f.name)
	if statErr != nil || pathErr != nil {
		return errors.Join(errors.New("claimed user output file is unavailable"), statErr, pathErr)
	}
	if !sameUserOutput(f.info, opened) || !sameUserOutput(f.info, current) ||
		f.info.Mode() != opened.Mode() || f.info.Mode() != current.Mode() {
		return errors.New("claimed user output file changed")
	}
	offset, err := f.File.Seek(0, io.SeekCurrent)
	if err != nil {
		return errors.Join(errors.New("claimed user output offset unavailable"), err)
	}
	if offset != f.info.Size() {
		return errors.New("claimed user output append offset changed")
	}
	return nil
}

func (f *userOutputFile) afterWrite(before os.FileInfo, count int) (os.FileInfo, error) {
	after, statErr := f.File.Stat()
	current, pathErr := f.root.Lstat(f.name)
	offset, offsetErr := f.File.Seek(0, io.SeekCurrent)
	if statErr != nil || pathErr != nil || offsetErr != nil {
		return nil, errors.Join(errors.New("claimed user output file unavailable after writing"), statErr, pathErr, offsetErr)
	}
	if !os.SameFile(before, after) || !sameUserOutput(after, current) || before.Mode() != after.Mode() ||
		after.Mode() != current.Mode() || after.Size()-before.Size() != int64(count) || offset != after.Size() {
		return nil, errors.New("claimed user output file changed during writing")
	}
	if err := f.env.checkUserRoot(f.root); err != nil {
		return nil, err
	}
	return after, nil
}

func (f *userOutputFile) Write(raw []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failure != nil {
		return 0, f.failure
	}
	if f.closed {
		return 0, io.ErrClosedPipe
	}
	if err := f.check(); err != nil {
		f.failure = err
		return 0, err
	}
	before := f.info
	n, writeErr := f.File.Write(raw)
	if writeErr == nil && n != len(raw) {
		writeErr = io.ErrShortWrite
	}
	after, proofErr := f.afterWrite(before, n)
	if proofErr == nil {
		f.info = after
	}
	f.failure = errors.Join(writeErr, proofErr)
	return n, f.failure
}

func (f *userOutputFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return f.failure
	}
	checkErr := f.check()
	closeErr, rootErr := f.File.Close(), f.root.Close()
	f.closed = true
	f.failure = errors.Join(f.failure, checkErr, closeErr, rootErr)
	return f.failure
}

func (e *Env) openUserOutput(name string) (io.WriteCloser, error) {
	if !userOutputName(name) {
		return nil, errors.New("user output name must be a local filename")
	}
	r, err := e.openUserRoot()
	if err != nil {
		return nil, err
	}
	f, err := r.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, errors.Join(err, r.Close())
	}
	if err := e.checkUserRoot(r); err != nil {
		// Preserve the new file for inspection, including an empty file.
		return nil, errors.Join(err, f.Close(), r.Close())
	}
	info, statErr := f.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		return nil, errors.Join(errors.New("new claimed user output changed"), statErr, f.Close(), r.Close())
	}
	output := &userOutputFile{File: f, root: r, env: e, name: name, info: info}
	if err := output.check(); err != nil {
		return nil, errors.Join(err, f.Close(), r.Close())
	}
	return output, nil
}

func writeUserBytes(f io.WriteCloser, raw []byte) error {
	n, err := f.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	return errors.Join(err, f.Close())
}

func (e *Env) writeUserOutput(name string, raw []byte) error {
	f, err := e.openUserOutput(name)
	if err != nil {
		return err
	}
	return writeUserBytes(f, raw)
}

func (e *Env) prepareUserOutput() (result error) {
	r, err := e.openUserRoot()
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, r.Close()) }()
	if _, err := r.Lstat("state.json"); !os.IsNotExist(err) {
		return errors.New("unexpected claimed user state.json; existing evidence is preserved")
	}
	if err := r.Mkdir("runs", 0o755); err != nil {
		return err
	}
	return e.checkUserRoot(r)
}

// State updates use the same opened regular file, without truncating it before
// identity and retained-byte checks. This is not an atomic or durable update.
func (e *Env) writeUserState(raw []byte) (result error) {
	r, err := e.openUserRoot()
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, r.Close()) }()
	previous := e.userRun.state
	flags := os.O_RDWR
	if previous == nil {
		flags |= os.O_CREATE | os.O_EXCL
	} else {
		before, err := r.Lstat("state.json")
		if err != nil || !sameUserOutput(previous, before) {
			return errors.New("claimed user state.json changed; existing evidence is preserved")
		}
	}
	f, err := r.OpenFile("state.json", flags, 0o644)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, f.Close()) }()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || previous != nil && !sameUserOutput(previous, opened) {
		return errors.New("claimed user state.json changed before writing")
	}
	if previous != nil {
		retained, err := io.ReadAll(io.LimitReader(f, previous.Size()+1))
		if err != nil || int64(len(retained)) != previous.Size() || sha256.Sum256(retained) != e.userRun.stateSHA256 {
			return errors.New("claimed user state.json bytes changed; existing evidence is preserved")
		}
	}
	current, err := r.Lstat("state.json")
	if err != nil || !sameUserOutput(opened, current) {
		return errors.New("claimed user state.json changed before writing")
	}
	if err := e.checkUserRoot(r); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	n, err := f.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return err
	}
	if err := f.Truncate(int64(len(raw))); err != nil {
		return err
	}
	after, err := f.Stat()
	current, pathErr := r.Lstat("state.json")
	if err != nil || pathErr != nil || !sameUserOutput(after, current) {
		return errors.New("claimed user state.json changed during writing")
	}
	if err := e.checkUserRoot(r); err != nil {
		return err
	}
	e.userRun.state, e.userRun.stateSHA256 = after, sha256.Sum256(raw)
	return nil
}
