//go:build linux

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ateomcgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/agent-substrate/substrate/internal/roottest"
	"golang.org/x/sys/unix"
)

func TestProtectedStopRejectsNonKernelAndMissingEvidence(t *testing.T) {
	root := t.TempDir()
	if err := KillAndReapLeafUnder(context.Background(), root, "missing"); err == nil {
		t.Fatal("missing cgroup accepted")
	}
	if err := os.Mkdir(filepath.Join(root, "fake"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := KillAndReapLeafUnder(context.Background(), root, "fake"); err == nil {
		t.Fatal("ordinary directory accepted as proof")
	}
	if err := reapStoppedProcess(context.Background(), 1<<30); err == nil {
		t.Fatal("invalid pidfd accepted")
	}
}

// This is a real kernel cgroup/process proof; it does not emulate runsc or a VM.
func TestProtectedStopKillsAndReapsOwnedProcessTree(t *testing.T) {
	roottest.Require(t, "creates private child cgroup and kills its owned processes")
	root := ownCgroup(t)
	name := fmt.Sprintf("protected-stop-%d", os.Getpid())
	leaf, err := openActorLeaf(root, name, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	defer func() { _ = removeActorLeaf(root, name) }()
	var previous int32
	if err := unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&previous)), 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, uintptr(previous), 0, 0, 0) }()
	ready := filepath.Join(t.TempDir(), "child")
	cmd := exec.Command("sh", "-c", "sleep 300 & echo $! > \"$1\"; wait", "stop-fixture", ready)
	cmd.SysProcAttr = leaf.SysProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	var child int
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(ready)
		child, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		if child > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if child <= 0 {
		t.Fatal("child did not start")
	}
	before, err := os.ReadFile(filepath.Join(root, name, "cgroup.procs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("owned cgroup=%s parent=%d child=%d before=%s", name, cmd.Process.Pid, child, before)
	for _, pid := range []int{cmd.Process.Pid, child} {
		if !strings.Contains("\n"+string(before), fmt.Sprintf("\n%d\n", pid)) {
			t.Fatalf("process %d not in owned cgroup", pid)
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := KillAndReapLeafUnder(cancelled, root, name); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled proof: %v", err)
	}
	if err := unix.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatal("cancelled proof changed process")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := KillAndReapLeafUnder(ctx, root, name); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{cmd.Process.Pid, child} {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("process %d not reaped: %v", pid, err)
		}
	}
	after, err := os.ReadFile(filepath.Join(root, name, "cgroup.events"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after=%s; both exact processes reaped", after)
	if err := KillAndReapLeafUnder(ctx, root, name); err == nil {
		t.Fatal("empty cgroup minted fresh proof")
	}
}

func TestProtectedStopDoesNotMistakeLiveProcessForReaped(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	fd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := reapStoppedProcess(ctx, fd); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("live process accepted: %v", err)
	}
}

func TestProtectedStopRejectsNestedCgroup(t *testing.T) {
	roottest.Require(t, "creates a private nested cgroup")
	root := ownCgroup(t)
	name := fmt.Sprintf("protected-nested-%d", os.Getpid())
	path := filepath.Join(root, name)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	child := filepath.Join(path, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(child)
	if err := KillAndReapLeafUnder(context.Background(), root, name); err == nil || !strings.Contains(err.Error(), "nested") {
		t.Fatalf("nested cgroup accepted: %v", err)
	}
}
