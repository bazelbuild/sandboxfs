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
	"os"
	"os/exec"
	"regexp"
	"syscall"

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
