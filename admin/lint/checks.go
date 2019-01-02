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

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"github.com/bazelbuild/sandboxfs/internal/shell"
)

// checkLicense checks if the given file contains the necessary license information and returns an
// error if this is not true or if the check cannot be performed.
func checkLicense(workspaceDir string, file string) error {
	for _, pattern := range []string{
		`Copyright.*Google`,
		`Apache License.*2.0`,
	} {
		matched, err := shell.Grep(pattern, file)
		if err != nil {
			return fmt.Errorf("license check failed for %s: %v", file, err)
		}
		if !matched {
			return fmt.Errorf("license check failed for %s: %s not found", file, pattern)
		}
	}

	return nil
}

// checkNoTabs checks if the given file contains any tabs as indentation and, if it does, returns
// an error.
func checkNoTabs(workspaceDir string, file string) error {
	input, err := os.OpenFile(file, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open %s for read: %v", file, err)
	}
	defer input.Close()

	preg := regexp.MustCompile(`^ *\t`)

	reader := bufio.NewReader(input)
	lineNo := 1
	done := false
	for !done {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			done = true
			// Fall through to process the last line in case it's not empty (when the
			// file didn't end with a newline).
		} else if err != nil {
			return fmt.Errorf("no tabs check failed for %s: %v", file, err)
		}
		if preg.MatchString(line) {
			return fmt.Errorf("no tabs check failed for %s: indentation tabs found at line %d", file, lineNo)
		}
		lineNo++
	}

	return nil
}

// runLinter is a helper function to run a linter that prints diagnostics to stdout and returns true
// even when the given files are not compliant.  The arguments indicate the full command line to
// run, including the path to the tool as the first argument.  The file to check is expected to
// appear as the last argument.
func runLinter(toolName string, arg ...string) error {
	file := arg[len(arg)-1]

	var output bytes.Buffer
	cmd := exec.Command(toolName, arg...)
	cmd.Stdout = &output
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("%s check failed for %s: %v", toolName, file, err)
	}
	if output.Len() > 0 {
		fmt.Printf("%s does not pass %s:\n", file, toolName)
		fmt.Println(output.String())
		return fmt.Errorf("%s check failed for %s: not compliant", toolName, file)
	}
	return nil
}

// checkGoFmt checks if the given file is formatted according to gofmt and, if not, prints a diff
// detailing what's wrong with the file to stdout and returns an error.
func checkGofmt(workspaceDir string, file string) error {
	return runLinter("gofmt", "-d", "-e", "-s", file)
}

// checkGoLint checks if the given file passes golint checks and, if not, prints diagnostic messages
// to stdout and returns an error.
func checkGolint(workspaceDir string, file string) error {
	// Lower confidence levels raise a per-file warning to remind about having a package-level
	// docstring... but the warning is issued blindly without checking for the existing of this
	// docstring in other packages.
	minConfidenceFlag := "-min_confidence=0.3"

	return runLinter("golint", minConfidenceFlag, file)
}

// checkAll runs all possible checks on a file.  Returns true if all checks pass, and false
// otherwise.  Error details are dumped to stderr.
func checkAll(workspaceDir string, file string) bool {
	isBuildFile := filepath.Base(file) == "BUILD.bazel" || filepath.Ext(file) == ".bzl" || filepath.Base(file) == "Makefile.in"

	// If a file starts with an upper-case letter, assume it's supporting package documentation
	// (all those files in the root directory) and avoid linting it.
	isDocumentation := mustMatch(`^[A-Z]`, filepath.Base(file)) && !isBuildFile

	log.Printf("Linting file %s", file)
	ok := true

	runCheck := func(checker func(string, string) error, file string) {
		if err := checker(workspaceDir, file); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", file, err)
			ok = false
		}
	}

	if !isBuildFile && !isDocumentation && filepath.Base(file) != "settings.json.in" {
		runCheck(checkLicense, file)
	}

	if filepath.Ext(file) == ".go" {
		runCheck(checkGofmt, file)
		runCheck(checkGolint, file)
	} else if !isBuildFile {
		runCheck(checkNoTabs, file)
	}

	return ok
}

// mustMatch returns true if the given regular expression matches the string.  The regular
// expression is assumed to be valid.
func mustMatch(pattern string, str string) bool {
	matched, err := regexp.MatchString(pattern, str)
	if err != nil {
		panic("invalid regexp")
	}
	return matched
}
