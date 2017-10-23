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

package sandbox

import (
	"syscall"
	"time"
)

// Atime obtains the access time from a system-specific stat structure.
func Atime(s *syscall.Stat_t) time.Time {
	return timespecToTime(s.Atimespec)
}

// Ctime obtains the inode change time from a system-specific stat structure.
func Ctime(s *syscall.Stat_t) time.Time {
	return timespecToTime(s.Ctimespec)
}

// Mtime obtains the modification time from a system-specific stat structure.
func Mtime(s *syscall.Stat_t) time.Time {
	return timespecToTime(s.Mtimespec)
}
