package fleet

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const userJSONLLineLimit = 16 << 20
const userJSONLDepthLimit = 128
const userJSONLTokenLimit = 1_000_000
const userReportWindowLimit = 10_000

var errUserInput = errors.New("invalid user input")

type UserInputFile struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256,omitempty"`
	Stable bool   `json:"stable"`
}

type UserInputTail struct {
	Line     int64  `json:"line"`
	Offset   int64  `json:"offset"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
	Accepted bool   `json:"accepted"`
}

type UserJSONLInput struct {
	UserInputFile
	Rows     int64          `json:"rows"`
	UsedRows int64          `json:"usedRows"`
	Tail     *UserInputTail `json:"tail,omitempty"`
}

type UserInputEvidence struct {
	Records       UserJSONLInput           `json:"records"`
	Samples       UserJSONLInput           `json:"samples"`
	Metadata      map[string]UserInputFile `json:"metadata"`
	Partial       bool                     `json:"partial"`
	RunCompletion string                   `json:"runCompletion"`
}

type userInputHash struct {
	h hash.Hash
	n int64
}

func (h *userInputHash) Write(raw []byte) (int, error) {
	n, err := h.h.Write(raw)
	h.n += int64(n)
	return n, err
}

func userToken(dec *json.Decoder, count *int) (json.Token, error) {
	*count += 1
	if *count > userJSONLTokenLimit {
		return nil, errUserInput
	}
	token, err := dec.Token()
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return token, err
}

func userJSONContents(dec *json.Decoder, opening json.Delim, depth int, count *int) error {
	if depth >= userJSONLDepthLimit {
		return errUserInput
	}
	keys := map[string]bool{}
	for dec.More() {
		if opening == '{' {
			key, err := userToken(dec, count)
			name, ok := key.(string)
			if err != nil {
				return err
			}
			if !ok || keys[name] {
				return errUserInput
			}
			keys[name] = true
		}
		value, err := userToken(dec, count)
		if err != nil {
			return err
		}
		if delimiter, nested := value.(json.Delim); nested {
			if delimiter != '{' && delimiter != '[' {
				return errUserInput
			}
			if err := userJSONContents(dec, delimiter, depth+1, count); err != nil {
				return err
			}
		}
	}
	closing, err := userToken(dec, count)
	if err != nil {
		return err
	}
	want := json.Delim('}')
	if opening == '[' {
		want = ']'
	}
	if closing != want {
		return errUserInput
	}
	return nil
}

func userJSONObject(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	count := 0
	first, err := userToken(dec, &count)
	if err != nil {
		return err
	}
	if first != json.Delim('{') {
		return errUserInput
	}
	if err := userJSONContents(dec, '{', 0, &count); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF || !utf8.Valid(raw) {
		return errUserInput
	}
	return nil
}

func userRecord(raw []byte) (*Record, error) {
	if err := userJSONObject(raw); err != nil {
		return nil, err
	}
	var record *Record
	if decodeMeasurementJSON(bytes.NewReader(raw), &record) != nil || record == nil || record.T <= 0 || record.User < 0 ||
		record.Action == "" || record.LatencyMS < 0 || record.ConfirmMS < 0 || record.IncludeMS < 0 || record.Bytes < 0 || record.OK && record.Reverted {
		return nil, errUserInput
	}
	if _, known := profileByName(record.Profile); !known {
		return nil, errUserInput
	}
	switch record.Kind {
	case "tx", "read", "order", "payout":
		return record, nil
	default:
		return nil, errUserInput
	}
}

func userSample(raw []byte) (*Sample, error) {
	if err := userJSONObject(raw); err != nil {
		return nil, err
	}
	var sample *Sample
	if decodeMeasurementJSON(bytes.NewReader(raw), &sample) != nil || sample == nil || sample.T <= 0 {
		return nil, errUserInput
	}
	return sample, nil
}

// scanUserJSONL observes bytes once and never repairs its input. Only an
// unterminated final object with incomplete syntax can leave a usable prefix.
func scanUserJSONL(reader io.Reader, source string, each func([]byte) error) (UserJSONLInput, error) {
	evidence := UserJSONLInput{UserInputFile: UserInputFile{Name: source, State: "empty"}}
	digest := &userInputHash{h: sha256.New()}
	buffer := bufio.NewReaderSize(io.TeeReader(reader, digest), 64<<10)
	var line []byte
	var offset, number int64
	for {
		part, readErr := buffer.ReadSlice('\n')
		if len(line)+len(part) > userJSONLLineLimit+1 {
			return evidence, fmt.Errorf("%s: line %d exceeds the JSONL row limit", source, number+1)
		}
		line = append(line, part...)
		if readErr == bufio.ErrBufferFull {
			continue
		}
		if readErr != nil && readErr != io.EOF {
			return evidence, fmt.Errorf("%s: input read failed", source)
		}
		if len(line) == 0 && readErr == io.EOF {
			break
		}
		number++
		terminated := line[len(line)-1] == '\n'
		payload := line
		if terminated {
			payload = payload[:len(payload)-1]
		}
		if len(payload) > userJSONLLineLimit || len(bytes.TrimSpace(payload)) == 0 {
			return evidence, fmt.Errorf("%s: line %d is not a usable JSONL row", source, number)
		}
		rowErr := each(payload)
		if !terminated {
			last := sha256.Sum256(line)
			evidence.Tail = &UserInputTail{Line: number, Offset: offset, Bytes: int64(len(line)), SHA256: hex.EncodeToString(last[:]), Accepted: rowErr == nil}
		}
		if rowErr != nil {
			if !terminated && readErr == io.EOF && errors.Is(rowErr, io.ErrUnexpectedEOF) && bytes.HasPrefix(bytes.TrimSpace(payload), []byte("{")) {
				evidence.State = "truncated-tail"
				break
			}
			return evidence, fmt.Errorf("%s: line %d is not a usable JSONL row", source, number)
		}
		evidence.Rows++
		evidence.State = "terminated"
		if !terminated {
			evidence.State = "unterminated"
		}
		offset += int64(len(line))
		line = line[:0]
		if readErr == io.EOF {
			break
		}
	}
	evidence.Bytes, evidence.SHA256 = digest.n, hex.EncodeToString(digest.h.Sum(nil))
	return evidence, nil
}

func openUserInput(path string) (*os.File, os.FileInfo, error) {
	selected, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !selected.Mode().IsRegular() {
		return nil, nil, errUserInput
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(selected, opened) {
		file.Close()
		return nil, nil, errUserInput
	}
	return file, opened, nil
}

func stableUserInput(path string, file *os.File, before os.FileInfo, bytesRead int64) bool {
	after, err := file.Stat()
	selected, pathErr := os.Lstat(path)
	return err == nil && pathErr == nil && selected.Mode().IsRegular() && os.SameFile(before, after) && os.SameFile(after, selected) &&
		before.Size() == bytesRead && before.Size() == after.Size() && before.ModTime() == after.ModTime()
}

func readUserJSONL(path string, optional bool, each func([]byte) error) (UserJSONLInput, error) {
	source := filepath.Base(path)
	file, before, err := openUserInput(path)
	if optional && os.IsNotExist(err) {
		return UserJSONLInput{UserInputFile: UserInputFile{Name: source, State: "missing"}}, nil
	}
	if err != nil {
		return UserJSONLInput{}, fmt.Errorf("%s: regular input file is unavailable", source)
	}
	defer file.Close()
	evidence, err := scanUserJSONL(file, source, each)
	if err != nil {
		return evidence, err
	}
	if !stableUserInput(path, file, before, evidence.Bytes) {
		return evidence, fmt.Errorf("%s: input changed while reading", source)
	}
	evidence.Stable = true
	return evidence, nil
}

func readUserMetadata(path string, out any) (UserInputFile, error) {
	source := filepath.Base(path)
	evidence := UserInputFile{Name: source, State: "missing"}
	file, before, err := openUserInput(path)
	if os.IsNotExist(err) {
		return evidence, nil
	}
	if err != nil {
		return evidence, fmt.Errorf("%s: regular metadata file is unavailable", source)
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil || !utf8.Valid(raw) || decodeMeasurementJSON(bytes.NewReader(raw), out) != nil {
		return evidence, fmt.Errorf("%s: invalid metadata", source)
	}
	if !stableUserInput(path, file, before, int64(len(raw))) {
		return evidence, fmt.Errorf("%s: metadata changed while reading", source)
	}
	digest := sha256.Sum256(raw)
	evidence.State, evidence.Bytes, evidence.SHA256, evidence.Stable = "observed", int64(len(raw)), hex.EncodeToString(digest[:]), true
	return evidence, nil
}

func loadUserContext(dir string, summary *UsersSummary) error {
	summary.Inputs.RunCompletion = "not-established"
	summary.Inputs.Metadata = map[string]UserInputFile{}
	var meta *struct {
		Label string         `json:"label"`
		Mix   []ProfileCount `json:"mix"`
	}
	info, err := readUserMetadata(filepath.Join(dir, "users.json"), &meta)
	if err != nil {
		return err
	}
	if info.State != "missing" {
		if meta == nil {
			return fmt.Errorf("users.json: invalid metadata object")
		}
		for _, profile := range meta.Mix {
			if _, known := profileByName(profile.Profile.Name); !known || profile.Count < 0 {
				return fmt.Errorf("users.json: invalid profile count")
			}
		}
		summary.Label, summary.Mix = meta.Label, meta.Mix
	}
	summary.Inputs.Metadata["users.json"] = info
	files, err := filepath.Glob(filepath.Join(dir, "*-users-*.json"))
	if err != nil {
		return fmt.Errorf("user phase metadata selection failed")
	}
	if len(files) == 0 {
		return nil
	}
	var phase *PhaseRecord
	info, err = readUserMetadata(files[len(files)-1], &phase)
	if err != nil || info.State == "missing" || phase == nil {
		return fmt.Errorf("user phase metadata is unavailable or invalid")
	}
	for _, key := range []string{"simStart", "simEnd"} {
		if value, present := phase.Data[key]; present {
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("user phase has invalid %s", key)
			}
			if _, err := time.Parse(time.RFC3339Nano, text); err != nil {
				return fmt.Errorf("user phase has invalid %s", key)
			}
		}
	}
	summary.Phase = phase
	summary.Inputs.Metadata[info.Name] = info
	return nil
}

func renderUserInputs(out *strings.Builder, inputs UserInputEvidence) {
	out.WriteString("\n## Retained input evidence\n\n")
	if inputs.Partial {
		out.WriteString("**PARTIAL INPUT.** These tables use only the retained valid prefix and usable observations. Missing or unfinished input does not establish a complete run.\n\n")
	}
	out.WriteString("File hashes and EOF observations describe per-file snapshots; run completion and a common atomic cross-file snapshot are not established. Raw inputs are unchanged.\n\n| Input | State | Accepted rows | Used rows | Raw bytes | SHA-256 |\n|---|---|---:|---:|---:|---|\n")
	for _, input := range []UserJSONLInput{inputs.Records, inputs.Samples} {
		fmt.Fprintf(out, "| %s | %s | %d | %d | %d | %s |\n", input.Name, input.State, input.Rows, input.UsedRows, input.Bytes, input.SHA256)
	}
	for _, input := range []UserJSONLInput{inputs.Records, inputs.Samples} {
		if tail := input.Tail; tail != nil {
			fmt.Fprintf(out, "\nUnterminated %s tail: line %d, offset %d, %d bytes, SHA-256 `%s`, accepted as a complete row: `%t`.\n\n", input.Name, tail.Line, tail.Offset, tail.Bytes, tail.SHA256, tail.Accepted)
		}
	}
}
