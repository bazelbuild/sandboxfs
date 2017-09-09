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

package integration

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelbuild/sandboxfs/internal/sandbox"
	"golang.org/x/sys/unix"
)

// jsonConfig converts a collection of sandbox mappings to the JSON structure expected by sandboxfs.
func jsonConfig(mappings []sandbox.MappingSpec) string {
	entries := make([]string, 0, len(mappings))
	for _, mapping := range mappings {
		entries = append(entries, fmt.Sprintf(`{"Mapping": "%s", "Target": "%s", "Writable": %v}`, mapping.Mapping, mapping.Target, mapping.Writable))
	}
	return fmt.Sprintf("[%s]\n\n", strings.Join(entries, ", "))
}

// reconfigure pushes a new configuration to the sandboxfs process and waits for acknowledgement.
func reconfigure(input io.Writer, output *bufio.Scanner, config string) error {
	n, err := io.WriteString(input, config)
	if err != nil {
		return fmt.Errorf("failed to send new configuration to sandboxfs: %v", err)
	}
	if n != len(config) {
		return fmt.Errorf("failed to send full configuration to sandboxfs: got %d bytes, want %d bytes", n, len(config))
	}

	if !output.Scan() {
		if err := output.Err(); err != nil {
			return fmt.Errorf("failed to read from sandboxfs's output: %v", err)
		}
		return fmt.Errorf("no data available in sandboxfs's output")
	}
	doneMarker := "Done"
	if output.Text() != doneMarker {
		return fmt.Errorf("sandboxfs did not ack configuration: got %s, want %s", output.Text(), doneMarker)
	}
	return nil
}

// doReconfigurationTest checks that reconfiguration works on an already-running sandboxfs instance
// given the handles for the input and output streams.  The way this works is by pushing a first
// configuration to sandboxfs, checking if the configuration was accepted properly, and then
// reconfiguring the file system in an "incompatible" manner to ensure the old file system contents
// are invalidated and the new ones are put in place.
func doReconfigurationTest(t *testing.T, state *mountState, input io.Writer, outputReader io.Reader) {
	output := bufio.NewScanner(outputReader)

	mkdirAllOrFatal(t, filepath.Join(state.root, "a/a"), 0755)
	config := jsonConfig([]sandbox.MappingSpec{
		sandbox.MappingSpec{Mapping: "/ro", Target: filepath.Join(state.root, "a/a"), Writable: false},
		sandbox.MappingSpec{Mapping: "/", Target: state.root, Writable: true},
		sandbox.MappingSpec{Mapping: "/ro/rw", Target: state.root, Writable: true},
	})
	if err := reconfigure(input, output, config); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(state.mountPoint, "ro/hello"), 0755); err == nil {
		t.Errorf("mkdir succeeded in read-only mapping")
	}
	if err := os.MkdirAll(filepath.Join(state.mountPoint, "ro/rw/hello"), 0755); err != nil {
		t.Errorf("mkdir failed in nested read-write mapping: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(state.mountPoint, "a/b/c"), 0755); err != nil {
		t.Errorf("mkdir failed in read-write root mapping: %v", err)
	}
	if err := ioutil.WriteFile(filepath.Join(state.mountPoint, "a/b/c/file"), []byte("foo bar"), 0644); err != nil {
		t.Errorf("write failed in read-write root mapping: %v", err)
	}
	if err := fileEquals(filepath.Join(state.mountPoint, "a/b/c/file"), "foo bar"); err != nil {
		t.Error(err)
	}
	if err := fileEquals(filepath.Join(state.root, "a/b/c/file"), "foo bar"); err != nil {
		t.Error(err)
	}

	config = jsonConfig([]sandbox.MappingSpec{
		sandbox.MappingSpec{Mapping: "/rw/dir", Target: state.root, Writable: true},
	})
	if err := reconfigure(input, output, config); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(state.mountPoint, "rw/dir/hello"), 0755); err != nil {
		t.Errorf("mkdir failed in read-write mapping: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(state.mountPoint, "a")); os.IsExist(err) {
		t.Errorf("old contents of root directory were not cleared after reconfiguration")
	}
	if _, err := os.Lstat(filepath.Join(state.mountPoint, "ro")); os.IsExist(err) {
		t.Errorf("old read-only mapping was not cleared after reconfiguration")
	}
}

// TODO(jmmv): Consider dropping stdin/stdout support as defaults.  This is quite an artificial
// construct and makes our testing quite complex.  Together with the idea of unifying static and
// dynamic commands, getting rid of the defaults may make more sense.
func TestReconfiguration_DefaultStreams(t *testing.T) {
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutReader.Close() // Just in case the test fails half-way through.
	defer stdoutWriter.Close() // Just in case the test fails half-way through.

	state := mountSetupWithOutputs(t, stdoutWriter, nil, "dynamic")
	defer state.tearDown(t)
	doReconfigurationTest(t, state, state.stdin, stdoutReader)
}

func TestReconfiguration_ExplicitStreams(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatalf("failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	inFifo := filepath.Join(tempDir, "input")
	if err := unix.Mkfifo(inFifo, 0600); err != nil {
		t.Fatalf("failed to create %s fifo: %v", inFifo, err)
	}

	outFifo := filepath.Join(tempDir, "output")
	if err := unix.Mkfifo(outFifo, 0600); err != nil {
		t.Fatalf("failed to create %s fifo: %v", outFifo, err)
	}

	state := mountSetupWithOutputs(t, nil, nil, "dynamic", "--input="+inFifo, "--output="+outFifo)
	defer state.tearDown(t)

	input, err := os.OpenFile(inFifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("failed to open input fifo for writing: %v", err)
	}
	defer input.Close()

	output, err := os.OpenFile(outFifo, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("failed to open output fifo for reading: %v", err)
	}
	defer output.Close()

	doReconfigurationTest(t, state, input, output)
}

func TestReconfiguration_StreamFileDoesNotExist(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatalf("failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	nonExistentFile := filepath.Join(tempDir, "non-existent/file")

	data := []struct {
		name string

		flag       string
		wantStderr string
	}{
		{
			"input",
			"--input=" + nonExistentFile,
			fmt.Sprintf("Unable to open file \"%s\" for reading: open %s: no such file or directory", nonExistentFile, nonExistentFile),
		},
		{
			"output",
			"--output=" + nonExistentFile,
			fmt.Sprintf("Unable to open file \"%s\" for writing: open %s: no such file or directory", nonExistentFile, nonExistentFile),
		},
	}
	for _, d := range data {
		t.Run(d.name, func(t *testing.T) {
			stdout, stderr, err := runAndWait(1, "dynamic", d.flag, filepath.Join(tempDir, "mnt"))
			if err != nil {
				t.Fatal(err)
			}
			if len(stdout) > 0 {
				t.Errorf("got %s; want stdout to be empty", stdout)
			}
			if !matchesRegexp(d.wantStderr, stderr) {
				t.Errorf("got %s; want stderr to match %s", stderr, d.wantStderr)
			}
		})
	}
}

// TODO(jmmv): Need to have tests for when the configuration is invalid (malformed JSON,
// inconsistent mappings, etc.).  No need for these to be very detailed given that the validations
// are already tested in "static" mode, but we must ensure that such validation paths are also
// exercised in dynamic mode when the configuration is processed.
