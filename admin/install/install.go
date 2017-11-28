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

// The install binary installs sandboxfs and all of its support files.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bazelbuild/sandboxfs/internal/shell"
)

const (
	// Relative path to the runfiles directory for the install script.  Used if the script is
	// run by hand from the workspace directory, without using "bazel run".
	runfiles = "bazel-bin/admin/install/install.runfiles/sandboxfs"

	// Path to the built sandboxfs binary relative to the runfiles directory.
	sandboxfsBin = "cmd/sandboxfs/sandboxfs"

	// Desired permissions for installed files.
	dataPerm = 0644
	execPerm = 0755
	dirPerm  = 0755
)

// installSpec contains the details for the installation of one source file.
type installSpec struct {
	// sourceFile contains the relative path to the source file to be installed.  If ran with
	// Bazel, all source files must be present in the runfiles directory of this tool, which
	// means they all must be listed as data dependencies.
	sourceFile string

	// targetDir indicates the relative path from the prefix where the file will be installed.
	// The name of the installed file matches the name of the source file.
	targetDir string

	// mode indicates the desired permissions of the installed file.
	mode os.FileMode
}

// doInstall performs the installation of all given files into the prefix.
func doInstall(sourceDir string, prefix string, filesToInstall []installSpec) error {
	for _, spec := range filesToInstall {
		sourceFile := filepath.Join(sourceDir, spec.sourceFile)

		targetDir := filepath.Join(prefix, spec.targetDir)
		if err := os.MkdirAll(targetDir, dirPerm); err != nil {
			return fmt.Errorf("failed to create %s: %v", targetDir, err)
		}

		targetFile := filepath.Join(targetDir, filepath.Base(spec.sourceFile))
		if err := shell.Install(sourceFile, targetFile, spec.mode); err != nil {
			return fmt.Errorf("failed to copy %s to %s: %v", spec.sourceFile, targetFile, err)
		}
	}
	return nil
}

// guessSourceDir returns the directory that contains the built files to be installed in the layout
// expected by the installData contents.
func guessSourceDir() (string, error) {
	// First check if the built sandboxfs binary is reachable from the current directory.  If it
	// is, we are running from within the runfiles directory via "blaze run".
	_, err := os.Stat(sandboxfsBin)
	if err != nil {
		// We aren't running via "blaze run".  Check and see if the built sandboxfs binary
		// is reachable from the blaze-bin directory and, if it is, use that.
		_, err := os.Stat(filepath.Join(runfiles, sandboxfsBin))
		if err != nil {
			return "", fmt.Errorf("cannot determine location of files to be installed, or was //cmd/sandboxfs not yet built?")
		}
		return runfiles, nil
	}
	return ".", nil
}

func main() {
	prefix := flag.String("prefix", "/usr/local", "Location where to install sandboxfs")
	flag.Parse()

	filesToInstall := []installSpec{
		{sandboxfsBin, "bin", execPerm},

		{"AUTHORS", "share/doc/sandboxfs", dataPerm},
		{"CONTRIBUTING.md", "share/doc/sandboxfs", dataPerm},
		{"CONTRIBUTORS", "share/doc/sandboxfs", dataPerm},
		{"INSTALL.md", "share/doc/sandboxfs", dataPerm},
		{"LICENSE", "share/doc/sandboxfs", dataPerm},
		{"NEWS.md", "share/doc/sandboxfs", dataPerm},
		{"README.md", "share/doc/sandboxfs", dataPerm},

		{"cmd/sandboxfs/sandboxfs.1", "share/man/man1", dataPerm},
	}

	sourceDir, err := guessSourceDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	if err := doInstall(sourceDir, *prefix, filesToInstall); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}
