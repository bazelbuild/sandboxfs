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

package integration

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"bazil.org/fuse"
)

func TestSignal_UnmountWhenCaught(t *testing.T) {
	for _, signal := range []os.Signal{syscall.SIGHUP, os.Interrupt, syscall.SIGTERM} {
		t.Run(signal.String(), func(t *testing.T) {
			state := mountSetup(t, "static", "-read_only_mapping=/:%ROOT%")
			defer state.tearDown(t)

			writeFileOrFatal(t, filepath.Join(state.root, "a"), 0644, "")
			if _, err := os.Lstat(filepath.Join(state.mountPoint, "a")); os.IsNotExist(err) {
				t.Fatalf("failed to create test file within file system: %v", err)
			}

			if err := state.cmd.Process.Signal(signal); err != nil {
				t.Fatalf("failed to deliver signal to sandboxfs process: %v", err)
			}
			if err := state.cmd.Wait(); err == nil {
				t.Fatalf("wait of sandboxfs returned nil, want an error")
			}
			if state.cmd.ProcessState.Success() {
				t.Fatalf("exit status of sandboxfs returned success, want an error")
			}

			if err := fuse.Unmount(state.mountPoint); err == nil {
				t.Fatalf("mount point should have been released during signal handling but wasn't")
			}
			if _, err := os.Lstat(filepath.Join(state.mountPoint, "a")); os.IsExist(err) {
				t.Fatalf("file system not unmounted; test file still exists in mount point")
			}

			state.cmd = nil // Tell state.tearDown that we cleaned the mount point ourselves.
		})
	}
}
