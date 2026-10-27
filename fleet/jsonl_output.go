package fleet

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// jsonlOutput owns one buffered stream. Accepted rows are not an fsync or a
// crash-durability guarantee. A failure stops later rows and cancels its owner.
type jsonlOutput struct {
	mu          sync.Mutex
	out         io.WriteCloser
	w           *bufio.Writer
	failure     error
	closed      bool
	cancel      context.CancelCauseFunc
	phaseActive bool
}

type checkedJSONLWriter struct{ io.Writer }

func (w checkedJSONLWriter) Write(raw []byte) (int, error) {
	n, err := w.Writer.Write(raw)
	if n < 0 || n > len(raw) {
		return 0, errors.New("invalid JSONL write count")
	}
	if n != len(raw) && err == nil {
		err = io.ErrShortWrite
	}
	return n, err
}

func newJSONLOutput(out io.WriteCloser, cancel context.CancelCauseFunc) jsonlOutput {
	return jsonlOutput{out: out, w: bufio.NewWriterSize(checkedJSONLWriter{out}, 1<<16), cancel: cancel}
}

func (o *jsonlOutput) failLocked(operation string, err error) error {
	if err != nil && o.failure == nil {
		o.failure = fmt.Errorf("%s: %w", operation, err)
		if o.cancel != nil {
			o.cancel(o.failure)
		}
	}
	return o.failure
}

func (o *jsonlOutput) append(value any, flush bool) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return o.failLocked("JSONL write", io.ErrClosedPipe)
	}
	if o.failure != nil {
		return o.failure
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return o.failLocked("JSONL encode", err)
	}
	// Match the reader's frame limits before any part of this row reaches
	// the buffer. Standard marshaling above still owns its temporary memory.
	if len(raw) > userJSONLLineLimit {
		return o.failLocked("JSONL row", errors.New("encoded row exceeds the reader byte limit"))
	}
	if err := userJSONObject(raw); err != nil {
		return o.failLocked("JSONL row structure", err)
	}
	if o.w == nil {
		return o.failLocked("JSONL write", errors.New("writer unavailable"))
	}
	n, err := o.w.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return o.failLocked("JSONL write", err)
	}
	if err := o.w.WriteByte('\n'); err != nil {
		return o.failLocked("JSONL write", err)
	}
	if flush {
		return o.failLocked("JSONL flush", o.w.Flush())
	}
	return nil
}

func (o *jsonlOutput) flush() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed && o.w != nil {
		o.failLocked("JSONL flush", o.w.Flush())
	}
	return o.failure
}

func (o *jsonlOutput) status() (closed bool, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.closed, o.failure
}

func (o *jsonlOutput) bindPhase(cancel context.CancelCauseFunc) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cancel, o.phaseActive = cancel, cancel != nil
	if cancel != nil {
		if o.closed {
			o.failLocked("JSONL stream", io.ErrClosedPipe)
		}
		if o.failure != nil {
			cancel(o.failure)
		}
	}
}

func (o *jsonlOutput) finish() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return o.failure
	}
	if o.phaseActive {
		o.failLocked("JSONL stream closed during active phases", io.ErrClosedPipe)
	}
	o.closed = true
	// Even an encoding failure must flush any earlier accepted prefix.
	if o.w != nil {
		if err := o.w.Flush(); err != nil {
			o.failure = errors.Join(o.failure, fmt.Errorf("JSONL flush: %w", err))
		}
	}
	if o.out != nil {
		if err := o.out.Close(); err != nil {
			o.failure = errors.Join(o.failure, fmt.Errorf("JSONL close: %w", err))
		}
	}
	if o.failure != nil && o.cancel != nil {
		o.cancel(o.failure)
	}
	return o.failure
}
