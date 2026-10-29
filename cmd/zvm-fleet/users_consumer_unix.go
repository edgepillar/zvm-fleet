//go:build unix

package main

import (
	"os"
	"syscall"
)

// A FIFO substituted after Lstat must not block the regular-file check in open.
// Root confines resolution; capture checks the opened and final file identities.
func openUserReportConsumerFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
