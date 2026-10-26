package fleet

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const userRunFile = "user-run.json"

type userRunClaim struct {
	id           string
	dir, marker  os.FileInfo
	markerSHA256 [32]byte
	state        os.FileInfo
	stateSHA256  [32]byte
}

// StartUserRun claims a fresh output directory before Init can write to it.
// The random identifier correlates cooperating writers; it is not authentication.
func (e *Env) StartUserRun() (result error) {
	if !e.runMu.TryLock() {
		return errors.New("cannot claim user output during phase execution")
	}
	defer e.runMu.Unlock()
	if e.userRun != nil {
		return errors.New("user output already claimed")
	}
	if info, err := os.Lstat(e.Out); err == nil {
		if !info.IsDir() {
			return errors.New("user output must be a regular directory")
		}
		entries, err := os.ReadDir(e.Out)
		if err != nil || len(entries) != 0 {
			return errors.New("user output must be empty; existing evidence is preserved")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(e.Out, 0o755); err != nil {
		return err
	}
	dir, err := os.Lstat(e.Out)
	if err != nil || !dir.IsDir() {
		return errors.New("user output directory is unavailable")
	}
	root, err := os.OpenRoot(e.Out)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(dir, opened) {
		return errors.New("user output directory changed before its claim")
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return fmt.Errorf("creating user run identifier: %w", err)
	}
	id := hex.EncodeToString(token[:])
	raw, err := json.Marshal(struct {
		Schema int    `json:"schema"`
		Kind   string `json:"kind"`
		RunID  string `json:"runId"`
	}{1, "users", id})
	if err != nil {
		return err
	}
	f, err := root.OpenFile(userRunFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := writeUserBytes(f, raw); err != nil {
		return err
	}
	marker, err := root.Lstat(userRunFile)
	after, dirErr := os.Lstat(e.Out)
	directory, openErr := root.Open(".")
	if openErr != nil {
		return openErr
	}
	entries, readErr := directory.ReadDir(-1)
	readErr = errors.Join(readErr, directory.Close())
	if err != nil || dirErr != nil || readErr != nil || !os.SameFile(dir, after) ||
		!marker.Mode().IsRegular() || len(entries) != 1 || entries[0].Name() != userRunFile {
		return errors.New("user output changed during its claim; retained files require inspection")
	}
	e.userRun = &userRunClaim{id: id, dir: dir, marker: marker, markerSHA256: sha256.Sum256(raw)}
	return e.checkUserRun()
}

func (e *Env) userRunToken() string {
	if e.userRun == nil {
		return ""
	}
	return e.userRun.id
}

func (e *Env) checkUserRun() error {
	r, err := e.openUserRoot()
	if err != nil {
		return err
	}
	return r.Close()
}

type UserCohortEvidence struct {
	State           string `json:"state"`
	RunID           string `json:"runId,omitempty"`
	ContextComplete bool   `json:"contextComplete"`
}

type UserTimeEvidence struct {
	Basis                string    `json:"basis"`
	Start                time.Time `json:"start"`
	End                  time.Time `json:"end"`
	LastRetainedRecordMS int64     `json:"lastRetainedRecordMs"`
}

type userReportContext struct {
	names                                       Names
	sawLegacy                                   bool
	marker, users, namesFile, phase, phaseUsers bool
	start, end                                  *time.Time
}

func validUserRunID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func objectRunID(object map[string]any) (string, error) {
	for key := range object {
		if strings.EqualFold(key, "runId") && key != "runId" {
			return "", errors.New("runId must use its canonical field name")
		}
	}
	value, present := object["runId"]
	if !present {
		return "", nil
	}
	id, ok := value.(string)
	if !ok || !validUserRunID(id) {
		return "", errors.New("invalid declared run identifier")
	}
	return id, nil
}

func rowRunID(raw []byte) (string, error) {
	var object map[string]any
	if err := decodeMeasurementJSON(bytes.NewReader(raw), &object); err != nil {
		return "", err
	}
	return objectRunID(object)
}

func (s *UsersSummary) observeRunID(source, id string) error {
	if id == "" {
		if s.Inputs.Cohort.RunID != "" {
			return fmt.Errorf("%s: missing identifier in a declared run", source)
		}
		s.context.sawLegacy = true
		return nil
	}
	if !validUserRunID(id) || s.context.sawLegacy || s.Inputs.Cohort.RunID != "" && s.Inputs.Cohort.RunID != id {
		return fmt.Errorf("%s: inconsistent declared run identifier", source)
	}
	s.Inputs.Cohort.State, s.Inputs.Cohort.RunID = "declared-consistent", id
	return nil
}

func contextObject(dir, name string, s *UsersSummary) (map[string]any, error) {
	var object map[string]any
	info, err := readUserMetadata(filepath.Join(dir, name), &object)
	s.Inputs.Metadata[name] = info
	if err != nil || info.State == "missing" {
		return nil, err
	}
	id, err := objectRunID(object)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid run identifier", name)
	}
	if err := s.observeRunID(name, id); err != nil {
		return nil, err
	}
	return object, nil
}

func decodeContextObject(object map[string]any, into any) error {
	raw, err := json.Marshal(object)
	if err != nil {
		return err
	}
	return decodeMeasurementJSON(bytes.NewReader(raw), into)
}

func (s *UsersSummary) usePopulation(object map[string]any) error {
	var selected struct {
		Label string         `json:"label"`
		Mix   []ProfileCount `json:"mix"`
	}
	if decodeContextObject(object, &selected) != nil {
		return errors.New("invalid user population metadata")
	}
	seen := map[string]bool{}
	for _, pc := range selected.Mix {
		if _, known := profileByName(pc.Profile.Name); !known || seen[pc.Profile.Name] || pc.Count < 0 || pc.Count > 1000 {
			return errors.New("invalid or duplicate profile count")
		}
		seen[pc.Profile.Name] = true
	}
	s.Label, s.Mix = selected.Label, selected.Mix
	return nil
}

func loadUserContext(dir string, s *UsersSummary) error {
	s.Inputs.RunCompletion = "not-established"
	s.Inputs.Cohort.State = "legacy-unbound"
	s.Inputs.Metadata = map[string]UserInputFile{}
	s.context.names = V1Names()
	marker, err := contextObject(dir, userRunFile, s)
	if err != nil {
		return err
	}
	if marker != nil {
		if marker["schema"] != json.Number("1") || marker["kind"] != "users" || s.Inputs.Cohort.RunID == "" {
			return errors.New("invalid user run marker")
		}
		s.context.marker = true
	}
	meta, err := contextObject(dir, "users.json", s)
	if err != nil {
		return err
	}
	if meta != nil {
		if err := s.usePopulation(meta); err != nil {
			return err
		}
		s.context.users = true
	}
	names, err := contextObject(dir, "names.json", s)
	if err != nil {
		return err
	}
	if names != nil {
		s.context.names = Names{}
		if decodeContextObject(names, &s.context.names) != nil || s.context.names.Node == "" {
			return errors.New("names.json: invalid node profile")
		}
		s.context.namesFile = true
	}
	files, err := filepath.Glob(filepath.Join(dir, "*-users-*.json"))
	if err != nil || len(files) > 1 {
		return errors.New("user phase metadata selection is ambiguous")
	}
	if len(files) == 0 {
		return nil
	}
	name := filepath.Base(files[0])
	object, err := contextObject(dir, name, s)
	if err != nil || object == nil {
		return errors.New("user phase metadata is unavailable or invalid")
	}
	phase := new(PhaseRecord)
	if decodeContextObject(object, phase) != nil || phase.ID != strings.TrimSuffix(name, ".json") {
		return errors.New("user phase identifier does not match its file")
	}
	if users, present := phase.Data["users"]; present {
		copy, ok := users.(map[string]any)
		if !ok {
			return errors.New("user phase has invalid user metadata")
		}
		id, err := objectRunID(copy)
		if err != nil || s.observeRunID("phase user metadata", id) != nil {
			return errors.New("user phase has inconsistent run metadata")
		}
		if meta != nil {
			left, leftErr := json.Marshal(meta)
			right, rightErr := json.Marshal(copy)
			if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
				return errors.New("users.json does not match the phase user metadata")
			}
		} else if err := s.usePopulation(copy); err != nil {
			return err
		}
		s.context.phaseUsers = true
	} else if phase.RunID != "" {
		return errors.New("declared user phase is missing its user metadata")
	}
	for _, key := range []string{"simStart", "simEnd"} {
		if value, present := phase.Data[key]; present {
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("user phase has invalid %s", key)
			}
			at, err := time.Parse(time.RFC3339Nano, text)
			if err != nil {
				return fmt.Errorf("user phase has invalid %s", key)
			}
			if key == "simStart" {
				s.context.start = &at
			} else {
				s.context.end = &at
			}
		}
	}
	if s.context.end != nil && (s.context.start == nil || s.context.end.Before(*s.context.start)) {
		return errors.New("user phase has inconsistent simulation bounds")
	}
	if !phase.StartedAt.IsZero() && !phase.EndedAt.IsZero() && phase.EndedAt.Before(phase.StartedAt) ||
		s.context.start != nil && !phase.StartedAt.IsZero() && s.context.start.Before(phase.StartedAt) ||
		s.context.end != nil && !phase.EndedAt.IsZero() && s.context.end.After(phase.EndedAt) {
		return errors.New("user simulation bounds contradict the enclosing phase")
	}
	s.Phase, s.context.phase = phase, true
	return nil
}

func (s *UsersSummary) checkRecordContext(r *Record) error {
	if len(s.Mix) > 0 {
		base, found := 0, false
		for _, pc := range s.Mix {
			if r.User >= base && r.User-base < pc.Count {
				found = r.Profile == pc.Profile.Name
				break
			}
			base += pc.Count
		}
		if !found {
			return errors.New("record does not match the declared user roster")
		}
	}
	// Payout T is an L2 block timestamp, not a wall-clock action start.
	if r.Kind != "payout" && (s.context.start != nil && r.T < s.context.start.UnixMilli() ||
		s.context.end != nil && r.T > s.context.end.UnixMilli()) {
		return errors.New("record lies outside its declared simulation interval")
	}
	return nil
}

func userRate(value float64, known bool, precision int) string {
	if !known {
		return "n/a"
	}
	return fmt.Sprintf("%.*f", precision, value)
}
