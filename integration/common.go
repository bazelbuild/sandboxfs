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

// Package integration contains integration tests for sandboxfs.  These are designed to execute
// the built binary and treat it as a black box.
package integration

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"bazil.org/fuse"
)

const (
	// Maximum amount of time to wait for sandboxfs to come up and start serving.
	startupDeadlineSeconds = 10

	// Maximum amount of time to wait for sandboxfs to gracefully exit after an unmount.
	shutdownDeadlineSeconds = 5
)

// Gets the path to the sandboxfs binary.
func sandboxfsBinary() (string, error) {
	bin := os.Getenv("SANDBOXFS")
	if bin == "" {
		return "", fmt.Errorf("SANDBOXFS not defined in environment")
	}
	return bin, nil
}

// runState holds runtime information for an in-progress sandboxfs execution.
type runState struct {
	cmd *exec.Cmd
	out bytes.Buffer
	err bytes.Buffer
}

// run starts a background process to run sandboxfs and passes it the given arguments.
func run(arg ...string) (*runState, error) {
	bin, err := sandboxfsBinary()
	if err != nil {
		return nil, fmt.Errorf("cannot find sandboxfs binary: %v", err)
	}

	var state runState
	state.cmd = exec.Command(bin, arg...)
	state.cmd.Stdout = &state.out
	state.cmd.Stderr = &state.err
	if err := state.cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start %s with arguments %v: %v", bin, arg, err)
	}
	return &state, nil
}

// wait awaits for completion of the process started by run and checks its exit status.
// Returns the textual contents of stdout and stderr for further inspection.
func wait(state *runState, wantExitStatus int) (string, string, error) {
	err := state.cmd.Wait()
	if wantExitStatus == 0 {
		if err != nil {
			return state.out.String(), state.err.String(), fmt.Errorf("want sandboxfs to exit with status 0; got %v", err)
		}
	} else {
		status := err.(*exec.ExitError).ProcessState.Sys().(syscall.WaitStatus)
		if wantExitStatus != status.ExitStatus() {
			return state.out.String(), state.err.String(), fmt.Errorf("want sandboxfs to exit with status %d, got %v", wantExitStatus, status.ExitStatus())
		}
	}
	return state.out.String(), state.err.String(), nil
}

// runAndWait invokes sandboxfs with the given arguments and waits for termination.
func runAndWait(wantExitStatus int, arg ...string) (string, string, error) {
	state, err := run(arg...)
	if err != nil {
		return "", "", err
	}
	return wait(state, wantExitStatus)
}

// startBackground spawns sandboxfs with the given arguments and waits for the file system to be
// ready for serving.  The cookie parameter specifies the relative path of a file within the mount
// point that must exist in order to consider the file system to be up and running.
//
// Returns a handle on the spawned sandboxfs process.
func startBackground(cookie string, args ...string) (*exec.Cmd, error) {
	bin, err := sandboxfsBinary()
	if err != nil {
		return nil, fmt.Errorf("cannot find sandboxfs binary: %v", err)
	}

	// The sandboxfs command line syntax requires the mount point to appear at the end and we
	// control all callers of this function within the tests, so we know this is true.  If not,
	// well, we have a bug and the test will crash/fail.
	mountPoint := args[len(args)-1]

	cmd := exec.Command(bin, args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start %s with arguments %v: %v", bin, args, err)
	}
	cookiePath := filepath.Join(mountPoint, cookie)
	for tries := 0; tries < startupDeadlineSeconds; tries++ {
		_, err := os.Stat(cookiePath)
		if err == nil {
			// Found the cookie file; sandboxfs is up and running!
			return cmd, nil
		}
		time.Sleep(time.Second)
	}
	// Give up.  sandboxfs did't come up, so kill the process and clean up.  There is not much
	// we can do here if we encounter errors (e.g. we don't even know if the mount point was
	// initialized, so the unmount call may or may not fail) so just try to clean up as much as
	// possible.
	cmd.Process.Kill()
	cmd.Wait()
	fuse.Unmount(mountPoint)
	return nil, fmt.Errorf("file system failed to come up: %s not found", cookiePath)
}

// mountState holds runtime information for tests that execute sandboxfs in the background and
// need to interact with a temporary directory where external files can be placed, and with the
// mount point.
type mountState struct {
	// Base directory where the test places any files it creates.  Tests should not reference
	// this directly.
	tempDir string

	// Directory that tests can use to place files that will later be remapped into the
	// sandbox.
	root string

	// Directory where the sandboxfs instance is mounted.
	mountPoint string

	// Handle for the running sandboxfs instance.  Can be used by tests to get access to the
	// input and output of the process.
	cmd *exec.Cmd
}

// mountSetup initializes a test that wants to run sandboxfs in the background.
//
// args contains the list of arguments to pass to the sandboxfs *without* the mount point: the
// mount point is derived from a temporary directory created here and returned in the mountPoint
// field of the mountState structure.  Similarly, the arguments can use %ROOT% to reference the
// temporary directory created here in which they can place files to be exposed in the sandbox.
//
// This helper function receives a testing.T object because test setup for sandboxfs is complex and
// we want to keep the test cases themselves as concise as possible.  Any failures within this
// function are fatal.
//
// Callers must defer execution of mountState.teardown() immediately on return to ensure the
// background process and the mount point are cleaned up on test completion.
func mountSetup(t *testing.T, args ...string) *mountState {
	success := false

	tempDir, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatalf("failed to create temporary directory: %v", err)
	}
	defer func() {
		if !success {
			os.RemoveAll(tempDir)
		}
	}()
	root := filepath.Join(tempDir, "root")
	mountPoint := filepath.Join(tempDir, "mnt")

	mkdirAllOrFatal(t, root, 0755)
	mkdirAllOrFatal(t, mountPoint, 0755)

	realArgs := make([]string, 0, len(args)+1)
	for _, arg := range args {
		realArgs = append(realArgs, strings.Replace(arg, "%ROOT%", root, -1))
	}
	realArgs = append(realArgs, mountPoint)

	writeFileOrFatal(t, filepath.Join(root, ".cookie"), 0400, "")
	cmd, err := startBackground(".cookie", realArgs...)
	if err != nil {
		t.Fatalf("failed to start sandboxfs: %v", err)
	}

	// All operations that can fail are now done.  Setting success=true prevents any deferred
	// cleanup routines from running, so any code below this line must not be able to fail.
	success = true
	state := &mountState{
		tempDir:    tempDir,
		root:       root,
		mountPoint: mountPoint,
		cmd:        cmd,
	}
	return state
}

// tearDown unmounts the sandboxfs instance and cleans up any test files.
//
// Similarly to mountSetup, tearDown takes a testing.T object.  The reason here is slightly
// different though: because tearDown is scheduled to run with "defer", we require a mechanism to
// report test failures if any cleanup action fails, so getting access to the testing.T object as an
// argument is the simplest way of doing so.
func (s *mountState) tearDown(t *testing.T) {
	// Calling fuse.Unmount on the mount point causes the running sandboxfs process to stop
	// serving and to exit cleanly.  Note that fuse.Unmount is not an unmount(2) system call:
	// this can be run as an unprivileged user, so we needn't check for root privileges.
	if err := fuse.Unmount(s.mountPoint); err != nil {
		t.Errorf("failed to unmount sandboxfs instance during teardown: %v", err)
	}

	timer := time.AfterFunc(shutdownDeadlineSeconds*time.Second, func() {
		s.cmd.Process.Kill()
	})
	err := s.cmd.Wait()
	timer.Stop()
	if err != nil {
		t.Errorf("sandboxfs did not exit successfully during teardown: %v", err)
	}

	if err := os.RemoveAll(s.tempDir); err != nil {
		t.Errorf("failed to remove temporary directory %s during teardown: %v", s.tempDir, err)
	}
}

// mkdirAllOrFatal wraps os.MkdirAll and immediately fails the test case on failure.
// This is purely syntactic sugar to keep test setup short and concise.
func mkdirAllOrFatal(t *testing.T, path string, perm os.FileMode) {
	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatalf("failed to create directory %s: %v", path, err)
	}
}

// mkdirAllOrFatal wraps ioutil.WriteFile and immediately fails the test case on failure.
// This is purely syntactic sugar to keep test setup short and concise.
func writeFileOrFatal(t *testing.T, path string, perm os.FileMode, contents string) {
	if err := ioutil.WriteFile(path, []byte(contents), perm); err != nil {
		t.Fatalf("failed to create file %s: %v", path, err)
	}
}

// dirEquals checks if the contents of two directories are the same.  The equality check is based
// on the directory entry names and their modes.
func dirEquals(path1 string, path2 string) error {
	names := make([]map[string]os.FileMode, 2)
	for i, path := range []string{path1, path2} {
		dirents, err := ioutil.ReadDir(path)
		if err != nil {
			return fmt.Errorf("failed to read contents of directory %s: %v", path, err)
		}
		names[i] = make(map[string]os.FileMode, len(dirents))
		for _, dirent := range dirents {
			names[i][dirent.Name()] = dirent.Mode()
		}
	}
	if !reflect.DeepEqual(names[0], names[1]) {
		return fmt.Errorf("contents of directory %s do not match %s; want %v, got %v", path1, path2, names[0], names[1])
	}
	return nil
}

// fileEquals checks if a file matches the expected contents.
func fileEquals(path string, wantContents string) error {
	contents, err := ioutil.ReadFile(path)
	if err != nil {
		return err
	}
	if string(contents) != wantContents {
		return fmt.Errorf("file %s doesn't match expected contents: got '%s', want '%s'", path, contents, wantContents)
	}
	return nil
}

// matchesRegexp returns true if the given string s matches the pattern.
func matchesRegexp(pattern string, s string) bool {
	match, err := regexp.MatchString(pattern, s)
	if err != nil {
		// This function is intended to be used exclusively from tests, and as such we know
		// that the given pattern must be valid.  If it's not, we've got a bug in the code
		// that must be fixed: there is no point in returning this as an error.
		panic(fmt.Sprintf("invalid regexp %s: %v; this is a bug in the test code", pattern, err))
	}
	return match
}
