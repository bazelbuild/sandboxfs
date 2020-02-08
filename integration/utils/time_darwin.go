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
	"os"
	"syscall"
	"time"
)

// ZeroBtime indicates that the given timestamp could not be queried.
var ZeroBtime = time.Date(0, 0, 0, 0, 0, 0, 0, time.Local)

// Atime obtains the access time from a system-specific stat structure.
func Atime(s *syscall.Stat_t) time.Time {
	return time.Unix(int64(s.Atimespec.Sec), int64(s.Atimespec.Nsec))
}

// Btime obtains the birth time from a file.
func Btime(path string) (time.Time, error) {
	fileInfo, err := os.Lstat(path)
	if err != nil {
		return ZeroBtime, err
	}

	s := fileInfo.Sys().(*syscall.Stat_t)
	return time.Unix(int64(s.Birthtimespec.Sec), int64(s.Birthtimespec.Nsec)), nil
}

// Ctime obtains the inode change time from a system-specific stat structure.
func Ctime(s *syscall.Stat_t) time.Time {
	return time.Unix(int64(s.Ctimespec.Sec), int64(s.Ctimespec.Nsec))
}

// Mtime obtains the modification time from a system-specific stat structure.
func Mtime(s *syscall.Stat_t) time.Time {
	return time.Unix(int64(s.Mtimespec.Sec), int64(s.Mtimespec.Nsec))
}
