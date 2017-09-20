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
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"bazil.org/fuse"
	"golang.org/x/sys/unix"
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
			return state.out.String(), state.err.String(), fmt.Errorf("got %v; want sandboxfs to exit with status 0", err)
		}
	} else {
		status := err.(*exec.ExitError).ProcessState.Sys().(unix.WaitStatus)
		if wantExitStatus != status.ExitStatus() {
			return state.out.String(), state.err.String(), fmt.Errorf("got %v; want sandboxfs to exit with status %d", status.ExitStatus(), wantExitStatus)
		}
	}
	return state.out.String(), state.err.String(), nil
}

// runAndWait invokes sandboxfs with the given arguments and waits for termination.
//
// TODO(jmmv): We should extend this function to also take what the expectation are on stdout and
// stderr to remove a lot of boilerplate from the tests... but we should probably wait until Go's
// 1.9 t.Helper() feature is available so that we can actually report failures/errors from here.
func runAndWait(wantExitStatus int, arg ...string) (string, string, error) {
	state, err := run(arg...)
	if err != nil {
		return "", "", err
	}
	return wait(state, wantExitStatus)
}

// waitForFile waits until a given file exists with a timeout of deadlineSeconds.  Returns true if
// the file was found before the timer expired, or false otherwise.
func waitForFile(cookie string, deadlineSeconds int) bool {
	for tries := 0; tries < deadlineSeconds; tries++ {
		_, err := os.Lstat(cookie)
		if err == nil {
			return true
		}
		time.Sleep(time.Second)
	}
	return false
}

// startBackground spawns sandboxfs with the given arguments and waits for the file system to be
// ready for serving.  The cookie parameter specifies the relative path of a file within the mount
// point that must exist in order to consider the file system to be up and running; the cookie may
// be empty if such waiting is not desired (e.g. when running sandboxfs in dynamic mode).
//
// The stdout and stderr of the sandboxfs process are redirected to the objects given to the
// function.  Any of these objects can be set to nil, which causes the corresponding output to be
// discarded.
//
// Returns a handle on the spawned sandboxfs process and a pipe to send data to its stdin.
func startBackground(cookie string, stdout io.Writer, stderr io.Writer, args ...string) (*exec.Cmd, io.WriteCloser, error) {
	bin, err := sandboxfsBinary()
	if err != nil {
		return nil, nil, fmt.Errorf("cannot find sandboxfs binary: %v", err)
	}

	// The sandboxfs command line syntax requires the mount point to appear at the end and we
	// control all callers of this function within the tests, so we know this is true.  If not,
	// well, we have a bug and the test will crash/fail.
	mountPoint := args[len(args)-1]

	cmd := exec.Command(bin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create stdin pipe: %v", err)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("failed to start %s with arguments %v: %v", bin, args, err)
	}

	if cookie != "" {
		cookiePath := filepath.Join(mountPoint, cookie)
		if !waitForFile(cookiePath, startupDeadlineSeconds) {
			// Give up.  sandboxfs did't come up, so kill the process and clean up.
			// There is not much we can do here if we encounter errors (e.g. we don't
			// even know if the mount point was initialized, so the unmount call may or
			// may not fail) so just try to clean up as much as possible.
			stdin.Close()
			cmd.Process.Kill()
			cmd.Wait()
			fuse.Unmount(mountPoint)
			return nil, nil, fmt.Errorf("file system failed to come up: %s not found", cookiePath)
		}
	}

	return cmd, stdin, nil
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

	// Pipe connected to sandboxfs's stdin.  Most tests don't need to communicate with the
	// sandboxfs process via stdin, so it's fine to just ignore this.
	stdin io.WriteCloser
}

// isDynamic returns true if the arguments to run sandboxfs cause the instance to be configured in
// dynamic mode.
func isDynamic(args ...string) bool {
	for _, arg := range args {
		if arg == "dynamic" {
			return true
		}
	}
	return false
}

// createDirsRequiredByMappings inspects the flags that configure sandboxfs to extract the paths to
// the targetes of the mappings, and creates those paths.
func createDirsRequiredByMappings(root string, args ...string) error {
	for _, arg := range args {
		if !matchesRegexp("mapping=.*:"+root+"/", arg) {
			continue // Not a mapping.
		}
		fields := strings.Split(arg, ":")
		if len(fields) != 2 {
			// If we encounter more than two fields on a mapping flag, we have hit a bug
			// in our tests and this bug must be fixed: propagating an error makes no
			// sense.  In other words: this function applies heuristics to determine
			// which flags represent mappings and extracts values from those... and if
			// we fail to do this properly, the calling tests won't work at all.
			panic(fmt.Sprintf("recognized a mapping but found more fields than expected: %v", fields))
		}
		dir := fields[1]
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to mkdir %s: %v", dir, err)
		}
	}
	return nil
}

// mountSetup initializes a test that wants to run sandboxfs in the background and does not care
// about what sandboxfs may print to stdout and stderr.
//
// This is essentially the same as mountSetupWithOutputs but with stdout and stderr set to ignore
// any output.  See the documentation for this other function for further details.
func mountSetup(t *testing.T, args ...string) *mountState {
	return mountSetupWithOutputs(t, nil, nil, args...)
}

// mountSetupWithOutputs initializes a test that wants to run sandboxfs in the background and wants
// to inspect the contents of stdout and stderr.
//
// args contains the list of arguments to pass to the sandboxfs *without* the mount point: the
// mount point is derived from a temporary directory created here and returned in the mountPoint
// field of the mountState structure.  Similarly, the arguments can use %ROOT% to reference the
// temporary directory created here in which they can place files to be exposed in the sandbox.
//
// The stdout and stderr of the sandboxfs process are redirected to the objects given to the
// function.  Any of these objects can be set to nil, which causes the corresponding output to be
// discarded.
//
// This helper function receives a testing.T object because test setup for sandboxfs is complex and
// we want to keep the test cases themselves as concise as possible.  Any failures within this
// function are fatal.
//
// Callers must defer execution of mountState.teardown() immediately on return to ensure the
// background process and the mount point are cleaned up on test completion.
func mountSetupWithOutputs(t *testing.T, stdout io.Writer, stderr io.Writer, args ...string) *mountState {
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

	if err := createDirsRequiredByMappings(root, realArgs...); err != nil {
		t.Fatalf("failed to create directories required by mappings: %v", err)
	}

	var cmd *exec.Cmd
	var stdin io.WriteCloser
	if isDynamic(args...) {
		// "dynamic" mode starts without any mappings so we cannot wait for the file system
		// to come up using a cookie file: instead, the caller is responsible for pushing an
		// initial configuration to sandboxfs and then waiting for confirmation.
		//
		// TODO(jmmv): This is an heuristic, and, as such, is imprecise and annoying because
		// it obscures what's happening in the tests.  It'd be nice to inject whether the
		// invocation is dynamic or not when calling the mountSetup* functions, but that
		// would obscure all other callers for no good reason.  The better alternative is to
		// reconsider whether we need a dynamic mode at all: it is probably a good idea to
		// consolidate the static/dynamic duality and allow any sandboxfs to accept
		// reconfiguration (which would, in turn, make all of our code much simpler).
		cmd, stdin, err = startBackground("", stdout, stderr, realArgs...)
	} else {
		writeFileOrFatal(t, filepath.Join(root, ".cookie"), 0400, "")
		cmd, stdin, err = startBackground(".cookie", stdout, stderr, realArgs...)
	}
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
		stdin:      stdin,
	}
	return state
}

// tearDown unmounts the sandboxfs instance and cleans up any test files.
//
// Similarly to mountSetup, tearDown takes a testing.T object.  The reason here is slightly
// different though: because tearDown is scheduled to run with "defer", we require a mechanism to
// report test failures if any cleanup action fails, so getting access to the testing.T object as an
// argument is the simplest way of doing so.
//
// If tests wish to control the shutdown of the sandboxfs process, they can do so, but then they
// must set s.cmd to nil to tell tearDown to not clean up the process a second time.
func (s *mountState) tearDown(t *testing.T) {
	if s.cmd != nil {
		if err := s.stdin.Close(); err != nil {
			t.Errorf("failed to close sandboxfs's stdin pipe: %v", err)
		}

		// Calling fuse.Unmount on the mount point causes the running sandboxfs process to
		// stop serving and to exit cleanly.  Note that fuse.Unmount is not an unmount(2)
		// system call: this can be run as an unprivileged user, so we needn't check for
		// root privileges.
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

		s.cmd = nil
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
		return fmt.Errorf("contents of directory %s do not match %s; got %v, want %v", path1, path2, names[1], names[0])
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
