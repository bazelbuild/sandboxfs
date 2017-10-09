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
	"bytes"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"

	"bazil.org/fuse"
	"github.com/bazelbuild/sandboxfs/integration/utils"
)

// checkSignalHandled verifies that the given sandboxfs process exited with an error on receipt of
// a signal and that the mount point was truly unmounted.
func checkSignalHandled(state *utils.MountState) error {
	if err := state.Cmd.Wait(); err == nil {
		return fmt.Errorf("wait of sandboxfs returned nil, want an error")
	}
	if state.Cmd.ProcessState.Success() {
		return fmt.Errorf("exit status of sandboxfs returned success, want an error")
	}

	if err := fuse.Unmount(state.MountPath()); err == nil {
		return fmt.Errorf("mount point should have been released during signal handling but wasn't")
	}

	state.Cmd = nil // Tell state.TearDown that we cleaned the mount point ourselves.
	return nil
}

func TestSignal_RaceBetweenSignalSetupAndMount(t *testing.T) {
	// This is a race-condition test: we run the same test multiple times, each increasing the
	// time it takes for us to kill the subprocess.  The numbers here proved to be sufficient
	// during development to exercise various bugs, and with machines getting faster, they
	// should continue to be good.
	ok := true
	for delayMs := 2; ok && delayMs < 200; delayMs += 2 {
		ok = t.Run(fmt.Sprintf("Delay%v", delayMs), func(t *testing.T) {
			state := utils.MountSetup(t, "dynamic")
			defer state.TearDown(t)

			time.Sleep(time.Duration(delayMs) * time.Millisecond)

			if err := state.Cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatalf("Failed to deliver signal to sandboxfs process: %v", err)
			}
			if err := checkSignalHandled(state); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSignal_UnmountWhenCaught(t *testing.T) {
	for _, signal := range []os.Signal{syscall.SIGHUP, os.Interrupt, syscall.SIGTERM} {
		t.Run(signal.String(), func(t *testing.T) {
			stderr := new(bytes.Buffer)

			state := utils.MountSetupWithOutputs(t, nil, stderr, "static", "-read_only_mapping=/:%ROOT%")
			defer state.TearDown(t)

			utils.MustWriteFile(t, state.RootPath("a"), 0644, "")
			if _, err := os.Lstat(state.MountPath("a")); os.IsNotExist(err) {
				t.Fatalf("Failed to create test file within file system: %v", err)
			}

			if err := state.Cmd.Process.Signal(signal); err != nil {
				t.Fatalf("Failed to deliver signal to sandboxfs process: %v", err)
			}
			if err := checkSignalHandled(state); err != nil {
				t.Fatal(err)
			}
			if !utils.MatchesRegexp(fmt.Sprintf("caught signal.*%v", signal.String()), stderr.String()) {
				t.Errorf("Termination error message does not mention signal name; got %v", stderr)
			}

			if _, err := os.Lstat(state.MountPath("a")); os.IsExist(err) {
				t.Fatalf("File system not unmounted; test file still exists in mount point")
			}
		})
	}
}
