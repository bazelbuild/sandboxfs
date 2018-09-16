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

// The lint binary ensures the source tree is correctly formatted.
package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/bazelbuild/rules_go/go/tools/bazel"
)

// getWorkspaceDir finds the path to the workspace given a path to a WORKSPACE file (which could be
// a symlink as staged by Bazel).  The returned path is absolute.
func getWorkspaceDir(file string) (string, error) {
	fileInfo, err := os.Lstat(file)
	if err != nil {
		return "", fmt.Errorf("cannot find %s: %v", file, err)
	}

	var dir string
	if fileInfo.Mode()&os.ModeType == os.ModeSymlink {
		realFile, err := os.Readlink(file)
		if err != nil {
			return "", fmt.Errorf("cannot read link %s: %v", file, err)
		}
		dir = filepath.Dir(realFile)
	} else {
		dir = filepath.Dir(file)
	}

	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("cannot convert %s to an absolute path: %v", dir, err)
	}

	// We have computed the real path to the workspace.  Now... let's make sure that's true by
	// looking for a known file.
	fileInfo, err = os.Stat(filepath.Join(dir, "README.md"))
	if err != nil {
		return "", fmt.Errorf("cannot find README.md in workspace %s: %v", dir, err)
	}
	if fileInfo.Mode()&os.ModeType != 0 {
		return "", fmt.Errorf("workspace %s contents are not regular files", dir)
	}

	return dir, nil
}

// isBlacklisted returns true if the given filename should not be linted.  The candidate must be
// given as a relative path to the workspace directory.
func isBlacklisted(candidate string) bool {
	// Skip hidden files as we don't need to run checks on them.  (This is not strictly
	// true, but it's simpler this way for now and the risk is low given that the hidden
	// files we have are trivial.)
	if strings.HasPrefix(candidate, ".") {
		return true
	}

	// Skip the Rust build directory.
	if strings.HasPrefix(candidate, "target/") {
		return true
	}

	return false
}

// collectFiles scans the given directory recursively and returns the paths to all regular files
// within it as relative paths to the given directory.
func collectFiles(dir string) ([]string, error) {
	files := make([]string, 0)

	collector := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relative, err := filepath.Rel(dir, path)
		if err != nil {
			panic(fmt.Sprintf("%s is not within %s but it should have been", path, dir))
		}
		if info.Mode()&os.ModeType == 0 {
			files = append(files, relative)
		}

		return err
	}

	return files, filepath.Walk(dir, collector)
}

func main() {
	verbose := flag.Bool("verbose", false, "Enables extra logging")
	workspace := flag.String("workspace", "WORKSPACE", "Path to the WORKSPACE file where the source tree list; used to find source files (symlinks are followed) and to resolve relative paths to sources")
	flag.Parse()

	if *verbose {
		log.SetOutput(os.Stderr)
	} else {
		log.SetOutput(ioutil.Discard)
	}

	if err := bazel.EnterRunfiles("sandboxfs", "admin/lint", "lint", "admin/lint"); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	workspaceDir, err := getWorkspaceDir(*workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
		os.Exit(1)
	}

	var relFiles []string
	if len(flag.Args()) == 0 {
		log.Printf("Searching for source files in %s", workspaceDir)
		relFiles, err = collectFiles(workspaceDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
			os.Exit(1)
		}
	} else {
		for _, file := range flag.Args() {
			if filepath.IsAbs(file) {
				fmt.Fprintf(os.Stderr, "ERROR: Explicitly-provided file names must be relative; %s was not\n", file)
				os.Exit(1)
			}
		}
		relFiles = flag.Args()
	}

	files := make([]string, 0, len(relFiles))
	for _, file := range relFiles {
		if isBlacklisted(file) {
			log.Printf("Skipping linting of %s because it is blacklisted", file)
		} else {
			files = append(files, filepath.Join(workspaceDir, file))
		}
	}

	failed := false
	for _, file := range files {
		if !checkAll(file) {
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}
