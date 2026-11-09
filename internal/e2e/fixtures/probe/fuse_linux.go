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

package main

import (
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const fuseDevice = "/dev/fuse"

// fuseRootSize is the size the probe's FUSE server reports for its root. A
// stat that returns it proves a full round trip: the kernel mounted the
// filesystem, sent the request over /dev/fuse and used the probe's answer.
const fuseRootSize = 4242

// fuseResponse reports whether /dev/fuse exists and whether a FUSE mount
// served by the probe itself works.
type fuseResponse struct {
	Exists   bool   `json:"exists"`
	Major    uint32 `json:"major"`
	Minor    uint32 `json:"minor"`
	Mounted  bool   `json:"mounted"`
	RootSize int64  `json:"rootSize"`
	Error    string `json:"error,omitempty"`
}

// fuseProbe stats /dev/fuse. If it is a char device, it mounts a FUSE
// filesystem, stats the mount root and unmounts it again.
func fuseProbe(w http.ResponseWriter, _ *http.Request) {
	var out fuseResponse
	var st unix.Stat_t
	if err := unix.Stat(fuseDevice, &st); err == nil && st.Mode&unix.S_IFMT == unix.S_IFCHR {
		out.Exists = true
		out.Major, out.Minor = unix.Major(st.Rdev), unix.Minor(st.Rdev)
		out.RootSize, out.Error = mountFUSE()
		out.Mounted = out.Error == ""
	}
	writeJSON(w, out)
}

// mountFUSE mounts a FUSE filesystem that serveFUSE answers, and returns the
// size that a stat of its root reports.
func mountFUSE() (int64, string) {
	fd, err := unix.Open(fuseDevice, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, "open: " + err.Error()
	}
	dir, err := os.MkdirTemp("", "fuse")
	if err != nil {
		unix.Close(fd)
		return 0, "mkdir: " + err.Error()
	}
	defer os.Remove(dir)
	data := fmt.Sprintf("fd=%d,rootmode=40000,user_id=0,group_id=0", fd)
	if err := unix.Mount("probe", dir, "fuse", unix.MS_NOSUID|unix.MS_NODEV, data); err != nil {
		unix.Close(fd)
		return 0, "mount: " + err.Error()
	}
	// The unmount ends the connection, so serveFUSE's read fails and it
	// closes fd.
	go serveFUSE(fd)
	defer func() { _ = unix.Unmount(dir, unix.MNT_DETACH) }()

	// ponytail: a stat that hangs leaks this goroutine; the probe is a
	// throwaway test actor.
	type result struct {
		st  unix.Stat_t
		err error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		r.err = unix.Stat(dir, &r.st)
		done <- r
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return 0, "stat: " + r.err.Error()
		}
		return r.st.Size, ""
	case <-time.After(10 * time.Second):
		return 0, "stat: no answer in 10s"
	}
}

// FUSE protocol opcodes and sizes, from <linux/fuse.h>.
const (
	fuseGetattr     = 3
	fuseForget      = 2
	fuseInit        = 26
	fuseBatchForget = 42
	fuseInHeader    = 40
	fuseOutHeader   = 16
	fuseInitOut     = 64
	fuseAttrOut     = 104
)

// serveFUSE answers FUSE_INIT and FUSE_GETATTR, and ENOSYS for all other
// requests. It is the smallest server that lets the kernel stat the root.
func serveFUSE(fd int) {
	defer unix.Close(fd)
	le := binary.LittleEndian
	buf := make([]byte, 1<<17) // the kernel refuses a read buffer under 8KiB
	for {
		n, err := unix.Read(fd, buf)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n < fuseInHeader {
			return
		}
		opcode, unique := le.Uint32(buf[4:]), le.Uint64(buf[8:])
		var body []byte
		var errno int32
		switch opcode {
		case fuseForget, fuseBatchForget:
			continue // no reply
		case fuseInit:
			body = make([]byte, fuseInitOut)
			le.PutUint32(body[0:], 7)                                        // major
			le.PutUint32(body[4:], min(le.Uint32(buf[fuseInHeader+4:]), 31)) // minor
			le.PutUint32(body[8:], le.Uint32(buf[fuseInHeader+8:]))          // max_readahead
			le.PutUint32(body[20:], 4096)                                    // max_write
		case fuseGetattr:
			body = make([]byte, fuseAttrOut)
			attr := body[16:]                           // after attr_valid, attr_valid_nsec, dummy
			le.PutUint64(attr[0:], 1)                   // ino: FUSE_ROOT_ID
			le.PutUint64(attr[8:], fuseRootSize)        // size
			le.PutUint32(attr[60:], unix.S_IFDIR|0o755) // mode
			le.PutUint32(attr[64:], 2)                  // nlink
		default:
			errno = -int32(unix.ENOSYS)
		}
		out := make([]byte, fuseOutHeader+len(body))
		le.PutUint32(out[0:], uint32(len(out)))
		le.PutUint32(out[4:], uint32(errno))
		le.PutUint64(out[8:], unique)
		copy(out[fuseOutHeader:], body)
		if _, err := unix.Write(fd, out); err != nil {
			return
		}
	}
}
