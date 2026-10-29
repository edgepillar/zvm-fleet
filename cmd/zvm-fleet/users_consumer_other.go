//go:build !unix

package main

import (
	"errors"
	"os"
)

func openUserReportConsumerFile(root *os.Root, name string) (*os.File, error) {
	return nil, errors.New("checked user report consumption requires a supported Unix platform; existing verification remains available")
}
