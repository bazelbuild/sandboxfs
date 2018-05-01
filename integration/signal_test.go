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
	"bufio"
	"bytes"
	"fmt"
	"io"
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
			state := utils.MountSetup(t)
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

			state := utils.MountSetupWithOutputs(t, nil, stderr, "--mapping=ro:/:%ROOT%")
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

func TestSignal_QueuedWhileInUse(t *testing.T) {
	stderrReader, stderrWriter := io.Pipe()
	defer stderrReader.Close()
	defer stderrWriter.Close()
	stderr := bufio.NewScanner(stderrReader)

	state := utils.MountSetupWithOutputs(t, nil, stderrWriter, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	// Create a file under the root directory and open it via the mount point to keep the file
	// system busy.
	utils.MustWriteFile(t, state.RootPath("file"), 0644, "file contents")
	file, err := os.Open(state.MountPath("file"))
	if err != nil {
		t.Fatalf("Failed to open test file: %v", err)
	}
	defer file.Close()

	// Send signal.  The mount point is busy because we hold an open file descriptor.  While the
	// file system will receive the signal, it will not be able to exit cleanly.  Instead, we
	// expect it to continue running until we release the resources, at which point the signal
	// should be processed.
	if err := state.Cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("Failed to deliver signal to sandboxfs process: %v", err)
	}

	// Wait until sandboxfs has acknowledged the first receipt of the signal, but continue
	// consuming stderr output in the background to prevent stalling sandboxfs due to a full
	// pipe.
	received := make(chan bool)
	go func() {
		notified := false
		for {
			if !stderr.Scan() {
				break
			}
			os.Stderr.WriteString(stderr.Text() + "\n")
			if utils.MatchesRegexp("unmounting.*failed.*will retry", stderr.Text()) {
				if !notified {
					received <- true
					notified = true
				}
				// Continue running so that any additional contents to stderr are
				// consumed.  Otherwise, the sandboxfs could stall if the stderr
				// pipe's buffer filled up.
			}
		}
	}()
	_ = <-received
	t.Logf("sandboxfs saw the signal delivery; continuing test")

	// Now that we know that sandboxfs has seen the signal, verify that it continues to work
	// successfully.
	if err := utils.FileEquals(state.MountPath("file"), "file contents"); err != nil {
		t.Fatalf("Failed to verify file contents using handle opened before signal delivery: %v", err)
	}
	if err := os.Mkdir(state.MountPath("dir"), 0755); err != nil {
		t.Fatalf("Mkdir failed after signal reception; sandboxfs may have exited: %v", err)
	}

	// Release the open file.  This should cause sandboxfs to terminate within a limited amount
	// of time, so ensure it exited as expected.
	file.Close()
	if err := checkSignalHandled(state); err != nil {
		t.Fatal(err)
	}
}
