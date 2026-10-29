//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestUserReportConsumerFIFOReplacementDoesNotWait(t *testing.T) {
	dir, out := consumerPair(t, []byte("owned Markdown"), []byte("owned summary"))
	open := func(root *os.Root, name string) (*os.File, error) {
		if name == "USERS.md" {
			if err := os.Rename(out, filepath.Join(dir, "retained-markdown")); err != nil {
				return nil, err
			}
			if err := syscall.Mkfifo(out, 0o600); err != nil {
				return nil, err
			}
		}
		return openUserReportConsumerFile(root, name)
	}
	done := make(chan error, 1)
	go func() {
		got, err := readUsersReportWith(dir, out, userReportReadLimits{1024, 1024}, open)
		if got != nil {
			done <- nil
			return
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("substituted FIFO was accepted")
		}
	case <-time.After(2 * time.Second):
		// Release only this owned FIFO before reporting a blocking-open failure.
		fd, err := syscall.Open(out, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			syscall.Close(fd)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("owned FIFO reader did not terminate")
		}
		t.Fatal("checked consumer waited for a FIFO writer")
	}
}
