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

// Package shell contains utilities that mimic external command-line utilities.
package shell

import (
	"fmt"
	"io"
	"log"
	"os"
	"syscall"
)

// Install copies a file and sets desired permissions on the destination.  The permissions are set
// exactly as specified, without respecting the process' umask.
func Install(source string, target string, mode os.FileMode) error {
	log.Printf("%s -> %s (mode %v)", source, target, mode)

	input, err := os.OpenFile(source, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("cannot open %s for read: %v", source, err)
	}
	defer input.Close()

	// Clear the umask so that the possible file creation or the subsequent chmod we perform
	// below all respect the mode given by the user.
	oldmask := syscall.Umask(0)
	syscall.Umask(oldmask)

	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("cannot open %s for write: %v", target, err)
	}
	defer output.Close()

	// The OpenFile above may have truncated an existing file so we need to explicitly set the
	// permissions we want on the file to cover that case.
	if err := os.Chmod(target, mode); err != nil {
		os.Remove(target)
		return fmt.Errorf("failed to set permissions of %s to %v: %v", target, mode, err)
	}

	if _, err := io.Copy(output, input); err != nil {
		os.Remove(target)
		return fmt.Errorf("data copy failed from %s to %s: %v", source, target, err)
	}

	return nil
}
