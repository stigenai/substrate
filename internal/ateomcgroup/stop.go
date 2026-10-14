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
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// KillAndReapLeafUnder is the fail-closed termination boundary for protected
// supervisors. Unlike best-effort cleanup, a missing or already empty cgroup
// cannot establish that this runtime stopped the activation it hosted.
func KillAndReapLeafUnder(ctx context.Context, root, name string) error {
	dir, err := protectedLeaf(root, name)
	if err != nil {
		return err
	}
	defer dir.Close()
	// Keep the exact kernel directory open. A replaced path cannot substitute
	// another actor's processes or provide a false empty observation.
	pinned := fmt.Sprintf("/proc/self/fd/%d", dir.Fd())
	if err := ctx.Err(); err != nil {
		return err
	}
	// Freeze before pinning, so fork cannot escape the captured live process set.
	if err := writeExisting(filepath.Join(pinned, "cgroup.freeze"), "1"); err != nil {
		return err
	}
	defer func() { _ = writeExisting(filepath.Join(pinned, "cgroup.freeze"), "0") }()
	if err := waitCgroupEvent(ctx, pinned, "frozen 1"); err != nil {
		return err
	}
	fds, err := pinLeafProcesses(pinned)
	if err != nil {
		return err
	}
	defer closeProcessHandles(fds)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeExisting(filepath.Join(pinned, "cgroup.kill"), "1"); err != nil {
		return fmt.Errorf("kill protected actor cgroup: %w", err)
	}
	if err := waitCgroupEvent(ctx, pinned, "populated 0"); err != nil {
		return err
	}
	for _, fd := range fds {
		if err := reapStoppedProcess(ctx, fd); err != nil {
			return err
		}
	}
	return nil
}

func protectedLeaf(root, name string) (*os.File, error) {
	path, err := actorLeafPath(root, name)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open protected actor cgroup: %w", err)
	}
	dir := os.NewFile(uintptr(fd), path)
	var stat unix.Statfs_t
	if err := unix.Fstatfs(fd, &stat); err != nil {
		_ = dir.Close()
		return nil, err
	}
	if stat.Type != unix.CGROUP2_SUPER_MAGIC {
		_ = dir.Close()
		return nil, errors.New("protected actor requires cgroup v2")
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	// Both runtimes place their host processes directly in one actor leaf.
	for _, entry := range entries {
		if entry.IsDir() {
			_ = dir.Close()
			return nil, errors.New("protected actor has unexpected nested cgroups")
		}
	}
	return dir, nil
}

func closeProcessHandles(fds []int) {
	for _, fd := range fds {
		_ = unix.Close(fd)
	}
}

func pinLeafProcesses(path string) ([]int, error) {
	data, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
	if err != nil {
		return nil, err
	}
	pids := strings.Fields(string(data))
	if len(pids) == 0 || len(pids) > 4096 {
		return nil, errors.New("protected actor process set is empty or exceeds bound")
	}
	fds := make([]int, 0, len(pids))
	for _, value := range pids {
		pid, err := strconv.Atoi(value)
		if err != nil || pid <= 0 {
			closeProcessHandles(fds)
			return nil, errors.New("invalid protected actor process")
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			closeProcessHandles(fds)
			return nil, fmt.Errorf("pin protected actor process: %w", err)
		}
		fds = append(fds, fd)
	}
	return fds, nil
}

func waitCgroupEvent(ctx context.Context, path, expected string) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		events, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(events), "\n") {
			if line == expected {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func reapStoppedProcess(ctx context.Context, fd int) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		polls := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		_, err := unix.Poll(polls, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		// HUP means the exact pinned process has been reaped, including when
		// the shared child reaper collected it before this observer.
		if polls[0].Revents&unix.POLLHUP != 0 {
			return nil
		}
		if polls[0].Revents&unix.POLLIN != 0 {
			var info unix.Siginfo
			err := unix.Waitid(unix.P_PIDFD, fd, &info, unix.WEXITED|unix.WNOHANG, nil)
			if err == nil {
				return nil
			}
			if !errors.Is(err, unix.ECHILD) && !errors.Is(err, unix.EINTR) {
				return err
			}
			// A still-unreaped process may not yet have been adopted from its
			// dying parent. ECHILD alone is not proof; wait for HUP or adoption.
		} else if polls[0].Revents != 0 {
			return errors.New("invalid protected actor process handle")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
