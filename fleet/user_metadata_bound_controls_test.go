package fleet

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMetadataExactByteBudgetsRetainContextAndEvidence(t *testing.T) {
	master := filepath.Join(t.TempDir(), "retained-boundary")
	metadataBudgetFixture(t, master, int(userMetadataByteLimit), false)
	for _, name := range []string{userRunFile, "users.json", "names.json", "00-users-synthetic.json"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			if name == userRunFile {
				metadataBudgetFixture(t, path, int(userMetadataMarkerByteLimit), false)
			} else if err := os.Rename(master, path); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]any
			info, err := readUserMetadata(path, &object)
			if err != nil || !info.Stable || info.State != "observed" || info.Bytes != userMetadataLimit(name) || len(info.SHA256) != 64 {
				t.Fatal("exact metadata byte boundary lost stable evidence", err)
			}
			summary := &UsersSummary{}
			if err := loadUserContext(dir, summary); err != nil {
				t.Fatal("exact metadata byte boundary failed context loading", err)
			}
			metadataBudgetUnchanged(t, path, before)
			if name != userRunFile {
				if err := os.Rename(path, master); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestMetadataExactTokenBudgetRemainsReadable(t *testing.T) {
	count := userMetadataTokenLimit - 7
	raw := `{"t":1000,"extension":[` + strings.Repeat("0,", count-1) + `0]}`
	path := filepath.Join(t.TempDir(), "metadata.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	info, err := readUserMetadata(path, &object)
	if err != nil || info.State != "observed" || !info.Stable || info.Bytes != int64(len(raw)) {
		t.Fatal("exact metadata token boundary was refused", err)
	}
}

type metadataCountReader struct {
	reader io.Reader
	bytes  int
}

func (r *metadataCountReader) Read(raw []byte) (int, error) {
	n, err := r.reader.Read(raw)
	r.bytes += n
	return n, err
}

func TestMetadataStreamingBudgetStopsAtOneExtraByte(t *testing.T) {
	reader := &metadataCountReader{reader: strings.NewReader(strings.Repeat(" ", 4096))}
	raw, err := readUserMetadataBytes(reader, 64)
	if err == nil || len(raw) != 0 || reader.bytes != 65 {
		t.Fatal("metadata stream read past the one-byte growth probe")
	}
}

type metadataReadFailure struct{ cause error }

func (r metadataReadFailure) Read([]byte) (int, error) { return 0, r.cause }

func TestMetadataBoundedReadRetainsActualReadFailure(t *testing.T) {
	want := errors.New("synthetic metadata read failure")
	raw, err := readUserMetadataBytes(io.MultiReader(strings.NewReader(`{}`), metadataReadFailure{want}), 1024)
	if len(raw) != 0 || !errors.Is(err, want) {
		t.Fatal("bounded reader lost the real input failure or retained a partial result", err)
	}
}

func TestMetadataMissingAndExactNumbersRetainMeaning(t *testing.T) {
	dir := t.TempDir()
	target := map[string]any{"retained": "synthetic"}
	info, err := readUserMetadata(filepath.Join(dir, "missing.json"), &target)
	if err != nil || info.State != "missing" || len(target) != 1 {
		t.Fatal("optional missing metadata changed", err)
	}
	path := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(path, []byte(`{"exact":18446744073709551615,"zero":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	info, err = readUserMetadata(path, &object)
	if err != nil || !info.Stable || object["exact"] != json.Number("18446744073709551615") || object["zero"] != json.Number("0") {
		t.Fatal("metadata bounds changed exact numeric decoding", err)
	}
}
