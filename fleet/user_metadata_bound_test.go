package fleet

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

const metadataBudgetUnion = `{"schema":1,"kind":"users","runId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","label":"synthetic","mix":[],"Node":"synthetic-node","id":"00-users-synthetic","data":{"users":{"runId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","label":"synthetic","mix":[]}}}`

type metadataBudgetFill struct {
	zeros  bool
	offset int64
}

func (r *metadataBudgetFill) Read(raw []byte) (int, error) {
	for i := range raw {
		raw[i] = ' '
		if r.zeros {
			raw[i] = '0'
			if (r.offset+int64(i))%2 != 0 {
				raw[i] = ','
			}
		}
	}
	r.offset += int64(len(raw))
	return len(raw), nil
}

func metadataBudgetFixture(t *testing.T, path string, size int, tokens bool) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if tokens {
		_, err = f.WriteString(metadataBudgetUnion[:len(metadataBudgetUnion)-1] + `,"extension":[`)
		if err == nil {
			_, err = io.CopyN(f, &metadataBudgetFill{zeros: true}, 2*int64(userJSONLTokenLimit))
		}
		if err == nil {
			_, err = f.WriteString(`0]}`)
		}
	} else {
		_, err = f.WriteString(metadataBudgetUnion)
		if err == nil {
			_, err = io.CopyN(f, &metadataBudgetFill{}, int64(size-len(metadataBudgetUnion)))
		}
	}
	if err := errors.Join(err, f.Close()); err != nil {
		t.Fatal(err)
	}
}

func metadataBudgetUnchanged(t *testing.T, path string, before os.FileInfo) {
	t.Helper()
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		t.Error("metadata refusal changed retained file identity or metadata")
	}
}

func metadataBudgetOversizedRoles(t *testing.T, run func(*testing.T, string, string)) {
	t.Helper()
	// Reuse one large inode sequentially instead of writing a copy for every
	// ordinary context role. Each callback returns before it is renamed again.
	master := filepath.Join(t.TempDir(), "retained-fixture")
	metadataBudgetFixture(t, master, userJSONLLineLimit+1, false)
	for _, name := range []string{userRunFile, "users.json", "names.json", "00-users-synthetic.json"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			if name == userRunFile {
				metadataBudgetFixture(t, path, 1025, false)
			} else if err := os.Rename(master, path); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			run(t, dir, path)
			metadataBudgetUnchanged(t, path, before)
			if name != userRunFile {
				if err := os.Rename(path, master); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestMetadataReaderRefusesOversizedInputs(t *testing.T) {
	metadataBudgetOversizedRoles(t, func(t *testing.T, dir, path string) {
		target := map[string]any{"retained": "synthetic"}
		info, err := readUserMetadata(path, &target)
		if err == nil || info.State == "observed" || info.Stable || info.SHA256 != "" || info.Bytes != 0 {
			t.Error("oversized metadata became an observed decoded input")
		}
		if len(target) != 1 || target["retained"] != "synthetic" {
			t.Error("oversized metadata changed the caller decode target")
		}
	})
}

func TestUserContextRefusesOversizedMetadata(t *testing.T) {
	metadataBudgetOversizedRoles(t, func(t *testing.T, dir, path string) {
		summary := &UsersSummary{}
		if err := loadUserContext(dir, summary); err == nil {
			t.Error("oversized metadata was accepted by report context")
		}
		if info := summary.Inputs.Metadata[filepath.Base(path)]; info.Stable || info.State == "observed" {
			t.Error("refused context reported observed metadata")
		}
	})
}

func TestMetadataReaderRefusesExcessiveTokenWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "00-users-synthetic.json")
	metadataBudgetFixture(t, path, 0, true)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	target := map[string]any{"retained": "synthetic"}
	info, err := readUserMetadata(path, &target)
	if err == nil || info.Stable || info.State == "observed" {
		t.Error("excessive metadata token work was accepted by reader")
	}
	if len(target) != 1 || target["retained"] != "synthetic" {
		t.Error("excessive token input changed the decode target")
	}
	metadataBudgetUnchanged(t, path, before)
}

func TestUserContextRefusesExcessiveTokenWork(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "00-users-synthetic.json")
	metadataBudgetFixture(t, path, 0, true)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	summary := &UsersSummary{}
	if err := loadUserContext(dir, summary); err == nil {
		t.Error("excessive metadata token work was accepted by report context")
	}
	metadataBudgetUnchanged(t, path, before)
}
