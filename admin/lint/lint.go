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

// collectFiles scans the given directory recursively and returns the paths to all regular files
// within it.
func collectFiles(dir string) ([]string, error) {
	files := make([]string, 0)

	collector := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip hidden files as we don't need to run checks on them.  (This is not strictly
		// true, but it's simpler this way for now and the risk is low given that the hidden
		// files we have are trivial.)
		if strings.HasPrefix(path, dir+"/.") {
			return err
		}

		if info.Mode()&os.ModeType == 0 {
			files = append(files, path)
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

	workspaceDir, err := getWorkspaceDir(*workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
		os.Exit(1)
	}

	files := flag.Args()
	if len(files) == 0 {
		log.Printf("Searching for source files in %s", workspaceDir)
		allFiles, err := collectFiles(workspaceDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
			os.Exit(1)
		}
		files = allFiles
	}

	failed := false
	for _, arg := range files {
		file := arg
		if !filepath.IsAbs(file) {
			file = filepath.Join(workspaceDir, file)
		}
		if !checkAll(file) {
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}
