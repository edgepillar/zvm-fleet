package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

// This fixture has no shell, network client or general-purpose command API.
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "owned probe fixture refused the operation")
		os.Exit(2)
	}
}

func run(args []string) error {
	if len(args) == 1 && args[0] == "hold" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		<-ctx.Done()
		return nil
	}
	if len(args) == 2 && args[0] == "+x" && args[1] == "/tmp/zvm-probe" {
		info, err := os.Lstat(args[1])
		if err != nil || !info.Mode().IsRegular() || info.Size() > 96<<20 {
			return errors.New("invalid owned probe")
		}
		return os.Chmod(args[1], info.Mode().Perm()|0o111)
	}
	if len(args) != 1 {
		return errors.New("unsupported fixture operation")
	}
	if args[0] == "runtime-limits" {
		values := map[string]int64{}
		for name, path := range map[string]string{"memoryBytes": "/sys/fs/cgroup/memory.max", "swapBytes": "/sys/fs/cgroup/memory.swap.max", "pids": "/sys/fs/cgroup/pids.max"} {
			raw, err := os.ReadFile(path)
			if err != nil || len(raw) > 128 {
				return errors.New("cgroup limit unavailable")
			}
			value, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
			if err != nil || value < 0 {
				return errors.New("cgroup limit unavailable")
			}
			values[name] = value
		}
		cpu, err := os.ReadFile("/sys/fs/cgroup/cpu.max")
		if err != nil || len(cpu) > 128 {
			return errors.New("CPU limit unavailable")
		}
		fields := strings.Fields(string(cpu))
		if len(fields) != 2 {
			return errors.New("CPU limit unavailable")
		}
		for i, name := range []string{"cpuQuota", "cpuPeriod"} {
			value, err := strconv.ParseInt(fields[i], 10, 64)
			if err != nil || value <= 0 {
				return errors.New("CPU limit unavailable")
			}
			values[name] = value
		}
		status, err := os.ReadFile("/proc/self/status")
		if err != nil || len(status) > 16<<10 {
			return errors.New("process constraints unavailable")
		}
		caps := map[string]string{}
		for _, line := range strings.Split(string(status), "\n") {
			pair := strings.SplitN(line, ":", 2)
			if len(pair) == 2 {
				caps[pair[0]] = strings.TrimSpace(pair[1])
			}
		}
		var filesystem syscall.Statfs_t
		if syscall.Statfs("/", &filesystem) != nil {
			return errors.New("root mount constraint unavailable")
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			Limits                               map[string]int64 `json:"limits"`
			Effective, Bounding, NoNewPrivileges string
			RootReadOnly                         bool
		}{values, caps["CapEff"], caps["CapBnd"], caps["NoNewPrivs"], filesystem.Flags&syscall.MS_RDONLY != 0})
	}
	path, limit := "", int64(0)
	switch args[0] {
	case "probe-digest":
		path, limit = "/tmp/zvm-probe", 96<<20
	case "db-digest":
		path, limit = "/data/zvm.db", 1<<20
	default:
		return errors.New("unsupported fixture operation")
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > limit {
		return errors.New("invalid owned fixture file")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return errors.New("owned fixture file changed")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, limit+1))
	after, statErr := os.Lstat(path)
	if err != nil || statErr != nil || n != before.Size() || !os.SameFile(before, after) ||
		after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return errors.New("owned fixture file changed")
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		SHA256 string `json:"sha256"`
		Bytes  int64  `json:"bytes"`
		Mode   uint32 `json:"mode"`
	}{hex.EncodeToString(h.Sum(nil)), n, uint32(after.Mode().Perm())})
}
