// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License.  You may obtain a copy
// of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.  See the
// License for the specific language governing permissions and limitations
// under the License.

package utils

import (
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ZeroBtime indicates that the given timestamp could not be queried.
var ZeroBtime = time.Date(0, 0, 0, 0, 0, 0, 0, time.Local)

// Atime obtains the access time from a system-specific stat structure.
func Atime(s *syscall.Stat_t) time.Time {
	return time.Unix(int64(s.Atim.Sec), int64(s.Atim.Nsec))
}

// Btime obtains the birth time from a system-specific stat structure.
func Btime(path string) (time.Time, error) {
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &stx); err != nil {
		return ZeroBtime, err
	}
	if stx.Mask&unix.STATX_BTIME == 0 {
		return ZeroBtime, nil
	}
	return time.Unix(int64(stx.Btime.Sec), int64(stx.Btime.Nsec)), nil
}

// Ctime obtains the inode change time from a system-specific stat structure.
func Ctime(s *syscall.Stat_t) time.Time {
	return time.Unix(int64(s.Ctim.Sec), int64(s.Ctim.Nsec))
}

// Mtime obtains the modification time from a system-specific stat structure.
func Mtime(s *syscall.Stat_t) time.Time {
	return time.Unix(int64(s.Mtim.Sec), int64(s.Mtim.Nsec))
}
