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

	"github.com/bazelbuild/rules_go/go/tools/bazel"
	"github.com/bazelbuild/sandboxfs/internal/shell"
)

const (
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
func doInstall(prefix string, filesToInstall []installSpec) error {
	for _, spec := range filesToInstall {
		targetDir := filepath.Join(prefix, spec.targetDir)
		if err := os.MkdirAll(targetDir, dirPerm); err != nil {
			return fmt.Errorf("failed to create %s: %v", targetDir, err)
		}

		targetFile := filepath.Join(targetDir, filepath.Base(spec.sourceFile))
		if err := shell.Install(spec.sourceFile, targetFile, spec.mode); err != nil {
			return fmt.Errorf("failed to copy %s to %s: %v", spec.sourceFile, targetFile, err)
		}
	}
	return nil
}

func main() {
	prefix := flag.String("prefix", "/usr/local", "Location where to install sandboxfs")
	flag.Parse()

	if err := bazel.EnterRunfiles("sandboxfs", "admin/install", "install", "admin/install"); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	sandboxfsBin, ok := bazel.FindBinary("cmd/sandboxfs", "sandboxfs")
	if !ok {
		fmt.Fprintf(os.Stderr, "ERROR: Cannot find sandboxfs binary; has //cmd/sandboxfs been built?\n")
		os.Exit(1)
	}

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

	if err := doInstall(*prefix, filesToInstall); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}
