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
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

// runState holds runtime information for an in-progress sandboxfs execution.
type runState struct {
	cmd *exec.Cmd
	out bytes.Buffer
	err bytes.Buffer
}

// setRustEnv configures sandboxfs's logging via environment variables.
func setRustEnv(cmd *exec.Cmd) {
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "RUST_BACKTRACE=full")
	cmd.Env = append(cmd.Env, "RUST_LOG=info")
}

// run starts a background process to run sandboxfs and passes it the given arguments.
func run(arg ...string) (*runState, error) {
	bin := GetConfig().SandboxfsBinary

	var state runState
	state.cmd = exec.Command(bin, arg...)
	state.cmd.Stdout = &state.out
	state.cmd.Stderr = &state.err
	setRustEnv(state.cmd)
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
		if err == nil {
			return state.out.String(), state.err.String(), fmt.Errorf("got 0; want sandboxfs to exit with status %d", wantExitStatus)
		}
		status := err.(*exec.ExitError).ProcessState.Sys().(syscall.WaitStatus)
		if wantExitStatus != status.ExitStatus() {
			return state.out.String(), state.err.String(), fmt.Errorf("got %v; want sandboxfs to exit with status %d", status.ExitStatus(), wantExitStatus)
		}
	}
	return state.out.String(), state.err.String(), nil
}

// RunAndWait invokes sandboxfs with the given arguments and waits for termination.
//
// TODO(jmmv): We should extend this function to also take what the expectations are on stdout and
// stderr to remove a lot of boilerplate from the tests... but we should probably wait until Go's
// 1.9 t.Helper() feature is available so that we can actually report failures/errors from here.
func RunAndWait(wantExitStatus int, arg ...string) (string, string, error) {
	state, err := run(arg...)
	if err != nil {
		return "", "", err
	}
	return wait(state, wantExitStatus)
}

// retry runs the given action until either it succeeds or the given deadline expires.  If the
// deadline expires, returns the last encountered error from the action.  Prints the given message
// when an error is encountered.
func retry(action func() error, message string, deadlineSeconds int) error {
	var lastErr error
	for tries := 0; tries < deadlineSeconds*10; tries++ {
		lastErr = action()
		if lastErr == nil {
			return nil
		}
		if tries > 10 {
			// Avoid cluttering the logs for the first few retries.  It's normal for
			// the first attempt to not succeed, which results in warning messages
			// printed for every test when there is nothing really wrong.
			fmt.Fprintf(os.Stderr, "In retry attempt %d: %s: %v\n", tries, message, lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return lastErr
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
// The credentials of the sandboxfs process are set to user if not nil.  Note that the caller must
// be root if the given user is not nil.
//
// Returns a handle on the spawned sandboxfs process and a pipe to send data to its stdin.
func startBackground(cookie string, stdout io.Writer, stderr io.Writer, user *UnixUser, args ...string) (*exec.Cmd, io.WriteCloser, error) {
	bin := GetConfig().SandboxfsBinary

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
	SetCredential(cmd, user)
	setRustEnv(cmd)
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("failed to start %s with arguments %v: %v", bin, args, err)
	}

	if cookie != "" {
		cookiePath := filepath.Join(mountPoint, cookie)
		waitForCookie := func() error { return FileExistsAsUser(cookiePath, user) }
		if err := retry(waitForCookie, "waiting for cookie to appear in mount point", startupDeadlineSeconds); err != nil {
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

// MountState holds runtime information for tests that execute sandboxfs in the background and
// need to interact with a temporary directory where external files can be placed, and with the
// mount point.
type MountState struct {
	// Cmd holds the handle for the running sandboxfs instance.  Can be used by tests to get
	// access to the input and output of the process.
	Cmd *exec.Cmd

	// Stdin is the pipe connected to sandboxfs's stdin.  Most tests don't need to communicate
	// with the sandboxfs process via stdin, so it's fine to just ignore this.
	Stdin io.WriteCloser

	// stdout contains the output of sandboxfs if the caller didn't capture it.
	stdout *bytes.Buffer

	// stderr contains the error output of sandboxfs if the caller didn't capture it.
	stderr *bytes.Buffer

	// tempDir points to the base directory where the test places any files it creates.
	tempDir string

	// root points to the directory that tests can use to place files that will later be
	// remapped into the sandbox.
	root string

	// mountPoint points to the directory where the sandboxfs instance is mounted.
	mountPoint string

	// oldMask keeps track of the process umask to restore when the test completes.
	oldMask int
}

// MountPath joins all the given path components and constructs an absolute path within the test's
// mount point.
func (s *MountState) MountPath(arg ...string) string {
	return filepath.Join(s.mountPoint, filepath.Join(arg...))
}

// RootPath joins all the given path components and constructs an absolute path within the directory
// where the test can place files that will later be remapped into the sandbox.
func (s *MountState) RootPath(arg ...string) string {
	return filepath.Join(s.root, filepath.Join(arg...))
}

// TempPath joins all the given path components and constructs an absolute path to the base
// temporary directory of the test.  Tests should rarely need to use this and should prefer the use
// of MountPath and RootPath.
func (s *MountState) TempPath(arg ...string) string {
	return filepath.Join(s.tempDir, filepath.Join(arg...))
}

// createDirsRequiredByMappings inspects the flags that configure sandboxfs to extract the paths to
// the targetes of the mappings, and creates those paths.
func createDirsRequiredByMappings(root string, args ...string) error {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--mapping=") {
			continue // Not a mapping.
		}
		fields := strings.Split(arg, ":")
		if len(fields) != 3 {
			// If we encounter more than two fields on a mapping flag, we have hit a bug
			// in our tests and this bug must be fixed: propagating an error makes no
			// sense.  In other words: this function applies heuristics to determine
			// which flags represent mappings and extracts values from those... and if
			// we fail to do this properly, the calling tests won't work at all.
			panic(fmt.Sprintf("recognized a mapping but found more fields than expected: %v", fields))
		}
		dir := fields[2]
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to mkdir %s: %v", dir, err)
		}
	}
	return nil
}

// hasRootMapping inspects the flags that configure sandboxfs and returns true if they define a
// mapping for the sandbox's root directory.
func hasRootMapping(args ...string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--mapping=ro:/:") || strings.HasPrefix(arg, "--mapping=rw:/:") {
			return true
		}
	}
	return false
}

// MountSetup initializes a test that runs sandboxfs in the background with default settings.
//
// This is essentially the same as mountSetupFull with stdout and stderr set to the caller's outputs
// and with rootSetup and the user set to nil.  See the documentation for this other function for
// further details.
func MountSetup(t *testing.T, args ...string) *MountState {
	t.Helper()

	return mountSetupFull(t, os.Stdout, os.Stderr, nil, nil, args...)
}

// MountSetupWithRootSetup initializes a test that runs sandboxfs in the background and provides
// a mechanism to configure the root directory before sandboxfs is started.
//
// This is essentially the same as mountSetupFull with stdout and stderr set to the caller's
// outputs and with the user set to nil.  See the documentation for this other function for
// further details.
func MountSetupWithRootSetup(t *testing.T, rootSetup func(string) error, args ...string) *MountState {
	t.Helper()

	return mountSetupFull(t, os.Stdout, os.Stderr, nil, rootSetup, args...)
}

// MountSetupWithOutputs initializes a test that runs sandboxfs in the background with output
// redirections.
//
// This is essentially the same as mountSetupFull with stdout and stderr set to the caller's
// provided values and with rootSetup and the user set to nil.  See the documentation for this other
// function for further details.
func MountSetupWithOutputs(t *testing.T, stdout io.Writer, stderr io.Writer, args ...string) *MountState {
	t.Helper()

	return mountSetupFull(t, stdout, stderr, nil, nil, args...)
}

// MountSetupWithUser initializes a test that runs sandboxfs in the background with different
// credentials.
//
// This is essentially the same as mountSetupFull with stdout and stderr set to the caller's
// outputs, with rootSetup set to nil, and with the user set to the given value.  See the
// documentation for this other function for further details.
func MountSetupWithUser(t *testing.T, user *UnixUser, args ...string) *MountState {
	t.Helper()

	return mountSetupFull(t, os.Stdout, os.Stderr, user, nil, args...)
}

// mountSetupFull initializes a test that runs sandboxfs in the background.
//
// args contains the list of arguments to pass to the sandboxfs *without* the mount point: the
// mount point is derived from a temporary directory created here and returned in the mountPoint
// field of the MountState structure.  Similarly, the arguments can use %ROOT% to reference the
// temporary directory created here in which they can place files to be exposed in the sandbox.
//
// The stdout and stderr of the sandboxfs process are redirected to the objects given to the
// function.  If these objects are os.Stdout or os.Stderr, respectively, the corresponding output
// is captured and dumped at the end of the test on error.
//
// The sandboxfs process is started with the credentials of the calling user, unless the user field
// is not nil, in which case those credentials are used.
//
// rootSetup is an optional hook that runs once the temporary root directory is created but before
// sandboxfs is mounted.  This allows tests to stage files that mappings can later refer to via
// a %ROOT%-prefixed path.
//
// This helper function receives a testing.T object because test setup for sandboxfs is complex and
// we want to keep the test cases themselves as concise as possible.  Any failures within this
// function are fatal.
//
// Callers must defer execution of MountState.TearDown() immediately on return to ensure the
// background process and the mount point are cleaned up on test completion.
func mountSetupFull(t *testing.T, stdout io.Writer, stderr io.Writer, user *UnixUser, rootSetup func(string) error, args ...string) *MountState {
	t.Helper()

	success := false

	// Reset the test's umask to zero.  This allows tests to not care about how the umask
	// affects files, which can introduce subtle bugs in the tests themselves.
	oldMask := unix.Umask(0)
	defer func() {
		if !success {
			unix.Umask(oldMask)
		}
	}()

	tempDir, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer func() {
		if !success {
			os.RemoveAll(tempDir)
		}
	}()
	root := filepath.Join(tempDir, "root")
	mountPoint := filepath.Join(tempDir, "mnt")

	MustMkdirAll(t, root, 0755)
	MustMkdirAll(t, mountPoint, 0755)

	if user != nil {
		// Ensure all users can navigate through the temporary directory, which are often created with
		// strict permissions.
		if err := os.Chmod(tempDir, 0755); err != nil {
			t.Fatalf("Failed to change permissions of %s", tempDir)
		}

		// The mount point must be owned by the user that will mount the FUSE file system.
		if err := os.Chown(mountPoint, user.UID, user.GID); err != nil {
			t.Fatalf("Failed to change ownership of %s", mountPoint)
		}
	}

	realArgs := make([]string, 0, len(args)+1)
	for _, arg := range args {
		realArgs = append(realArgs, strings.Replace(arg, "%ROOT%", root, -1))
	}
	realArgs = append(realArgs, mountPoint)

	if err := createDirsRequiredByMappings(root, realArgs...); err != nil {
		t.Fatalf("Failed to create directories required by mappings: %v", err)
	}
	if rootSetup != nil {
		if err := rootSetup(root); err != nil {
			t.Fatalf("Failed to run custom rootSetup hook on %s: %v", root, err)
		}
	}

	var storedStdout *bytes.Buffer
	if stdout == os.Stdout {
		storedStdout = new(bytes.Buffer)
		stdout = storedStdout
	}

	var storedStderr *bytes.Buffer
	if stderr == os.Stderr {
		storedStderr = new(bytes.Buffer)
		stderr = storedStderr
	}

	var cmd *exec.Cmd
	var stdin io.WriteCloser
	if !hasRootMapping(realArgs...) {
		// Without a mapping at root, we can't wait for sandboxfs to come up using a cookie
		// file.  For now, we assume that only the reconfiguration tests do this as they get
		// the same wait functionality by pushing an initial configuration request.
		//
		// TODO(jmmv): We shouldn't need to do this.  Now that there is no more "static" and
		// "dynamic" sandbox types, we could define initial root mappings in all cases and
		// then let the reconfiguration take over.  This is tricky because sandboxfs blocks
		// when opening the input FIFO until there is a writer for it, which is not yet the
		// case for our tests.  And, with the work I'm planning to do on reconfigurations, I
		// may drop the possibility of changing the root mapping.
		cmd, stdin, err = startBackground("", stdout, stderr, user, realArgs...)
	} else {
		MustWriteFile(t, filepath.Join(root, ".cookie"), 0444, "")
		cmd, stdin, err = startBackground(".cookie", stdout, stderr, user, realArgs...)
		if err := os.Remove(filepath.Join(root, ".cookie")); err != nil {
			t.Errorf("Failed to delete the startup cookie file: %v", err)
			// Continue text execution.  Failing hard here is a difficult condition to
			// handle because sandboxfs is already running and we'd need to clean it up.
			// It's easier to just let the test run, and it's actually beneficial to do
			// so: many tests will work even if the removal failed, so the few tests
			// that fail will hint at to what may be wrong.
		}
		if err != nil {
			t.Fatalf("Failed to start sandboxfs: %v", err)
		}
	}

	// All operations that can fail are now done.  Setting success=true prevents any deferred
	// cleanup routines from running, so any code below this line must not be able to fail.
	success = true
	state := &MountState{
		Cmd:        cmd,
		Stdin:      stdin,
		stdout:     storedStdout,
		stderr:     storedStderr,
		tempDir:    tempDir,
		root:       root,
		mountPoint: mountPoint,
		oldMask:    oldMask,
	}
	return state
}

// TearDown unmounts the sandboxfs instance and cleans up any test files.
//
// Similarly to MountSetup, TearDown takes a testing.T object.  The reason here is slightly
// different though: because TearDown is scheduled to run with "defer", we require a mechanism to
// report test failures if any cleanup action fails, so getting access to the testing.T object as an
// argument is the simplest way of doing so.
//
// If tests wish to control the shutdown of the sandboxfs process, they can do so, but then they
// must set s.Cmd to nil to tell TearDown to not clean up the process a second time.  The same
// applies to s.Stdin.
//
// If tests wish to check if TearDown returned an error, they can do so by avoiding the recommended
// use of "defer".  Note, though, that such tests will only receive the first error encountered by
// this function, and that the function will run to completion even if there were failures.
func (s *MountState) TearDown(t *testing.T) error {
	t.Helper()

	var firstErr error
	setFirstErr := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	unix.Umask(s.oldMask)

	if s.Stdin != nil {
		if err := s.Stdin.Close(); err != nil {
			t.Errorf("Failed to close sandboxfs's stdin pipe: %v", err)
			setFirstErr(err)
		}

		s.Stdin = nil
	}

	if s.Cmd != nil {
		// Calling fuse.Unmount on the mount point causes the running sandboxfs process to
		// stop serving and to exit cleanly.  Note that fuse.Unmount is not an unmount(2)
		// system call: this can be run as an unprivileged user, so we needn't check for
		// root privileges.
		//
		// Note that we must be resilient to unmount failures as we can get transient
		// "resource busy" errors.  While our tests should not be keeping files open on the
		// mount point after they terminate (that'd be a bug), the operating system may
		// interfere with us and access the file system under the hood.  If that happens, we
		// get a business error even when we think the mount point is not busy.  For
		// example, on macOS, the Finder may decide to obtain information about the mount
		// point and, if it does that while we try to unmount it, we get an unexpected
		// error.
		unmount := func() error { return fuse.Unmount(s.mountPoint) }
		if err := retry(unmount, "waiting for file system to be unmounted", shutdownDeadlineSeconds); err != nil {
			t.Errorf("Failed to unmount sandboxfs instance during teardown: %v", err)
			setFirstErr(err)
		}

		timer := time.AfterFunc(shutdownDeadlineSeconds*time.Second, func() {
			s.Cmd.Process.Kill()
		})
		err := s.Cmd.Wait()
		timer.Stop()
		if err != nil {
			t.Errorf("sandboxfs did not exit successfully during teardown: %v", err)
			setFirstErr(err)
		}

		s.Cmd = nil
	}

	if t.Failed() {
		if s.stdout != nil {
			fmt.Fprintf(os.Stderr, "sandboxfs stdout was:\n%s", s.stdout.String())
		}
		if s.stderr != nil {
			fmt.Fprintf(os.Stderr, "sandboxfs stderr was:\n%s", s.stderr.String())
		}
	}

	if err := os.RemoveAll(s.tempDir); err != nil {
		t.Errorf("Failed to remove temporary directory %s during teardown: %v", s.tempDir, err)
		setFirstErr(err)
	}

	return firstErr
}
