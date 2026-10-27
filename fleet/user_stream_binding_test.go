package fleet

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

var streamBindingKinds = []string{"regular", "symlink", "missing", "directory", "truncate", "append", "mode", "mtime", "offset"}

type streamBindingView struct {
	path string
	info os.FileInfo
	raw  []byte
	link string
}

func streamBindingViews(t *testing.T, paths []string) []streamBindingView {
	t.Helper()
	var views []streamBindingView
	for _, path := range paths {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			views = append(views, streamBindingView{path: path})
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		view := streamBindingView{path: path, info: info}
		if info.Mode().IsRegular() {
			view.raw, err = os.ReadFile(path)
		} else if info.Mode()&os.ModeSymlink != 0 {
			view.link, err = os.Readlink(path)
		}
		if err != nil {
			t.Fatal(err)
		}
		views = append(views, view)
	}
	return views
}

func streamBindingPreserved(t *testing.T, views []streamBindingView) {
	t.Helper()
	for _, before := range views {
		info, err := os.Lstat(before.path)
		if before.info == nil {
			if !os.IsNotExist(err) {
				t.Error("refusal created a missing output name")
			}
			continue
		}
		if err != nil || !os.SameFile(before.info, info) || before.info.Mode() != info.Mode() {
			t.Error("refusal changed retained file identity or mode")
			continue
		}
		if info.Mode().IsRegular() {
			raw, err := os.ReadFile(before.path)
			if err != nil || !bytes.Equal(raw, before.raw) {
				t.Error("refusal changed retained or replacement bytes")
			}
		} else if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(before.path)
			if err != nil || link != before.link {
				t.Error("refusal changed a replacement link")
			}
		}
	}
}

func streamBindingChange(t *testing.T, e *Env, name, kind string, out io.WriteCloser) []streamBindingView {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("open-file replacement fixtures are qualified on POSIX only")
	}
	path := filepath.Join(e.Out, name)
	paths := []string{path}
	switch kind {
	case "regular", "symlink", "missing", "directory":
		retained := filepath.Join(e.Out, "retained-"+name)
		if err := os.Rename(path, retained); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, retained)
		if kind == "regular" {
			if err := os.WriteFile(path, []byte("foreign synthetic replacement\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		} else if kind == "symlink" {
			target := filepath.Join(t.TempDir(), "replacement-target")
			if err := os.WriteFile(target, []byte("foreign synthetic target\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			paths = append(paths, target)
		} else if kind == "directory" {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	case "truncate":
		if err := os.WriteFile(path, []byte("other\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "append":
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := f.WriteString("foreign synthetic append\n")
		if err := errors.Join(writeErr, f.Close()); err != nil {
			t.Fatal(err)
		}
	case "mode":
		if err := os.Chmod(path, 0o400); err != nil {
			t.Fatal(err)
		}
	case "mtime":
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		changed := info.ModTime().Add(time.Hour)
		if err := os.Chtimes(path, changed, changed); err != nil {
			t.Fatal(err)
		}
	case "offset":
		if _, err := out.(*userOutputFile).File.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unknown stream binding fixture")
	}
	return streamBindingViews(t, paths)
}

func streamBindingOpened(t *testing.T) (*Env, io.WriteCloser) {
	t.Helper()
	e := &Env{}
	boundaryClaim(t, e)
	f, err := e.openUserOutput("records.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if _, err := f.Write([]byte("retained synthetic prefix\n")); err != nil {
		t.Fatal(err)
	}
	return e, f
}

func TestClaimedStreamWriteRefusesChangedFileBinding(t *testing.T) {
	for _, kind := range streamBindingKinds {
		t.Run(kind, func(t *testing.T) {
			e, f := streamBindingOpened(t)
			views := streamBindingChange(t, e, "records.jsonl", kind, f)
			n, first := f.Write([]byte("later bytes\n"))
			if n != 0 || first == nil {
				t.Error("opened output accepted a displaced file or changed append state")
			}
			if n, again := f.Write([]byte("still later bytes\n")); n != 0 || again == nil {
				t.Error("opened output failure did not stop later writes")
			}
			if err := f.Close(); err == nil {
				t.Error("close omitted the displaced output failure")
			}
			streamBindingPreserved(t, views)
		})
	}
}

func TestClaimedStreamCloseRefusesChangedFileBinding(t *testing.T) {
	for _, kind := range streamBindingKinds {
		t.Run(kind, func(t *testing.T) {
			e, f := streamBindingOpened(t)
			views := streamBindingChange(t, e, "records.jsonl", kind, f)
			if err := f.Close(); err == nil {
				t.Error("clean close accepted a displaced file or changed append state")
			}
			streamBindingPreserved(t, views)
		})
	}
}

func TestClaimedSampleStreamFailureCancelsItsOwner(t *testing.T) {
	for _, kind := range streamBindingKinds {
		t.Run(kind, func(t *testing.T) {
			e := &Env{}
			boundaryClaim(t, e)
			closeStream, err := e.StreamSamples()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { closeStream() })
			if err := e.onSample(Sample{T: 1000}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			e.stream.bindPhase(cancel)
			views := streamBindingChange(t, e, "samples.jsonl", kind, e.stream.out)
			first := e.onSample(Sample{T: 2000})
			if first == nil || context.Cause(ctx) == nil {
				t.Error("changed sample file was accepted or did not cancel its owner")
			}
			if again := e.onSample(Sample{T: 3000}); again == nil {
				t.Error("sample output continued past its first refusal")
			}
			e.stream.bindPhase(nil)
			if err := closeStream(); err == nil {
				t.Error("sample close lost its file refusal")
			}
			streamBindingPreserved(t, views)
		})
	}
}

func TestClaimedRecordFlushFailureCancelsItsOwner(t *testing.T) {
	for _, kind := range streamBindingKinds {
		t.Run(kind, func(t *testing.T) {
			e := &Env{}
			boundaryClaim(t, e)
			f, err := e.openUserOutput("records.jsonl")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			stream := newJSONLOutput(f, cancel)
			defer stream.finish()
			row := Record{T: 1000, Profile: "light", Kind: "read", Action: "synthetic-read", OK: true}
			if err := stream.append(&row, true); err != nil {
				t.Fatal(err)
			}
			views := streamBindingChange(t, e, "records.jsonl", kind, f)
			row.T = 2000
			if err := stream.append(&row, false); err != nil {
				t.Fatal("row was not buffered before flush", err)
			}
			first := stream.flush()
			if first == nil || context.Cause(ctx) == nil {
				t.Error("changed record file was accepted or did not cancel its owner")
			}
			if err := stream.append(&row, true); err == nil {
				t.Error("record output continued past its first refusal")
			}
			if err := stream.finish(); err == nil {
				t.Error("record close lost its file refusal")
			}
			streamBindingPreserved(t, views)
		})
	}
}
