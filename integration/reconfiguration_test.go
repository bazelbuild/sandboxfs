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
	"syscall"
	"testing"

	"github.com/bazelbuild/sandboxfs/integration/utils"
	"github.com/bazelbuild/sandboxfs/internal/sandbox"
	"golang.org/x/net/context"
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
func doReconfigurationTest(t *testing.T, state *utils.MountState, input io.Writer, outputReader io.Reader) {
	output := bufio.NewScanner(outputReader)

	utils.MustMkdirAll(t, state.RootPath("a/a"), 0755)
	config := jsonConfig([]sandbox.MappingSpec{
		{Mapping: "/", Target: state.RootPath(), Writable: true},
		{Mapping: "/ro", Target: state.RootPath("a/a"), Writable: false},
		{Mapping: "/ro/rw", Target: state.RootPath(), Writable: true},
	})
	if err := reconfigure(input, output, config); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(state.MountPath("ro/hello"), 0755); err == nil {
		t.Errorf("Mkdir succeeded in read-only mapping")
	}
	if err := os.MkdirAll(state.MountPath("ro/rw/hello"), 0755); err != nil {
		t.Errorf("Mkdir failed in nested read-write mapping: %v", err)
	}
	if err := os.MkdirAll(state.MountPath("a/b/c"), 0755); err != nil {
		t.Errorf("Mkdir failed in read-write root mapping: %v", err)
	}
	if err := ioutil.WriteFile(state.MountPath("a/b/c/file"), []byte("foo bar"), 0644); err != nil {
		t.Errorf("Write failed in read-write root mapping: %v", err)
	}
	if err := utils.FileEquals(state.MountPath("a/b/c/file"), "foo bar"); err != nil {
		t.Error(err)
	}
	if err := utils.FileEquals(state.RootPath("a/b/c/file"), "foo bar"); err != nil {
		t.Error(err)
	}

	config = jsonConfig([]sandbox.MappingSpec{
		{Mapping: "/rw/dir", Target: state.RootPath(), Writable: true},
	})
	if err := reconfigure(input, output, config); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(state.MountPath("rw/dir/hello"), 0755); err != nil {
		t.Errorf("Mkdir failed in read-write mapping: %v", err)
	}
	if _, err := os.Lstat(state.MountPath("a")); os.IsExist(err) {
		t.Errorf("Old contents of root directory were not cleared after reconfiguration")
	}
	if _, err := os.Lstat(state.MountPath("ro")); os.IsExist(err) {
		t.Errorf("Old read-only mapping was not cleared after reconfiguration")
	}
}

// TODO(jmmv): Consider dropping stdin/stdout support as defaults.  This is quite an artificial
// construct and makes our testing quite complex.
func TestReconfiguration_DefaultStreams(t *testing.T) {
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutReader.Close() // Just in case the test fails half-way through.
	defer stdoutWriter.Close() // Just in case the test fails half-way through.

	state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr)
	defer state.TearDown(t)
	doReconfigurationTest(t, state, state.Stdin, stdoutReader)
}

func TestReconfiguration_ExplicitStreams(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	inFifo := filepath.Join(tempDir, "input")
	if err := syscall.Mkfifo(inFifo, 0600); err != nil {
		t.Fatalf("Failed to create %s fifo: %v", inFifo, err)
	}

	outFifo := filepath.Join(tempDir, "output")
	if err := syscall.Mkfifo(outFifo, 0600); err != nil {
		t.Fatalf("Failed to create %s fifo: %v", outFifo, err)
	}

	state := utils.MountSetupWithOutputs(t, nil, os.Stderr, "-input="+inFifo, "-output="+outFifo)
	defer state.TearDown(t)

	input, err := os.OpenFile(inFifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("Failed to open input fifo for writing: %v", err)
	}
	defer input.Close()

	output, err := os.OpenFile(outFifo, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("Failed to open output fifo for reading: %v", err)
	}
	defer output.Close()

	doReconfigurationTest(t, state, input, output)
}

func TestReconfiguration_RaceSystemComponents(t *testing.T) {
	// This test verifies that a dynamic sandboxfs instance can be unmounted immediately after
	// reconfiguration.
	//
	// When this test was originally conceived, it did not actually test for a sandboxfs problem
	// despite it being in the "reconfiguration" category.  This test was added as a check to
	// ensure that our own testing infrastructure can cope with system components (e.g. macOS's
	// Finder) interfering with the mount point while we are cleaning up the test state.  The
	// reason this exists under the "reconfiguration" category is because a
	// dynamically-configured sandboxfs instance is more subject to this problem than a
	// statically-configured one: during test setup in the former case, we cannot wait for
	// sandboxfs to be ready for serving, which means that the time window between the
	// reconfiguration operation and the unmount is smaller.  This shorter window makes the race
	// between us and the system easier to trigger.

	oneShot := func() error {
		stdoutReader, stdoutWriter := io.Pipe()
		defer stdoutReader.Close()
		defer stdoutWriter.Close()
		output := bufio.NewScanner(stdoutReader)

		state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr, "-mapping=ro:/:%ROOT%")
		// state.TearDown not deferred here because we want to explicitly control for any
		// possible error it may report and abort the whole test early in that case.

		utils.MustWriteFile(t, state.RootPath("first"), 0644, "First")

		firstConfig := jsonConfig([]sandbox.MappingSpec{
			{Mapping: "/first", Target: state.RootPath("first"), Writable: false},
		})
		if err := reconfigure(state.Stdin, output, firstConfig); err != nil {
			state.TearDown(t)
			return err
		}
		return state.TearDown(t)
	}

	for i := 0; i < 200; i++ {
		if err := oneShot(); err != nil {
			t.Fatalf("Failed after %d mount+reconfigure sequences: %v", i, err)
		}
	}
}

func TestReconfiguration_DirectoryListings(t *testing.T) {
	testData := []struct {
		name string

		dir                string
		firstConfigTarget  string
		secondConfigTarget string
		keepDirOpen        string
	}{
		{"MappedDir", "/mapped", "dir1", "dir2", ""},
		{"MappedDirAndKeepRootOpen", "/mapped", "dir1", "dir2", "/"},
		{"MappedDirAndKeepSelfOpen", "/mapped", "dir1", "dir2", "/mapped"},
		{"Root", "/", "dir1/first", "dir2/second", ""},
		{"RootAndKeepSelfOpen", "/", "dir1/first", "dir2/second", "/"},
		{"ScaffoldDir", "/scaffold", "dir1/first", "dir2/second", ""},
		{"ScaffoldDirAndKeepRootOpen", "/scaffold", "dir1/first", "dir2/second", "/"},
		{"ScaffoldDirAndKeepSelfOpen", "/scaffold", "dir1/first", "dir2/second", "/scaffold"},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			stdoutReader, stdoutWriter := io.Pipe()
			defer stdoutReader.Close()
			defer stdoutWriter.Close()
			output := bufio.NewScanner(stdoutReader)

			state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr)
			defer state.TearDown(t)

			utils.MustMkdirAll(t, state.RootPath("dir1"), 0755)
			utils.MustWriteFile(t, state.RootPath("dir1/first"), 0644, "First")
			utils.MustMkdirAll(t, state.RootPath("dir2"), 0755)
			utils.MustWriteFile(t, state.RootPath("dir2/second"), 0644, "Second")

			firstConfig := jsonConfig([]sandbox.MappingSpec{
				{Mapping: filepath.Join(d.dir, "first"), Target: state.RootPath(d.firstConfigTarget), Writable: false},
			})
			if err := reconfigure(state.Stdin, output, firstConfig); err != nil {
				t.Fatalf("First configuration failed: %v", err)
			}
			if err := utils.DirEntryNamesEqual(state.MountPath(d.dir), []string{"first"}); err != nil {
				t.Error(err)
			}

			if d.keepDirOpen != "" {
				// Keep a handle open to the directory for the duration of the test, which makes everything
				// more difficult to handle.  This ensures that no handles made stale during reconfiguration
				// are used.
				handle, err := os.OpenFile(state.MountPath(d.keepDirOpen), os.O_RDONLY, 0)
				if err != nil {
					t.Fatalf("Cannot open %s to keep directory busy: %v", d.keepDirOpen, err)
				}
				defer handle.Close()
			}

			secondConfig := jsonConfig([]sandbox.MappingSpec{
				{Mapping: filepath.Join(d.dir, "second"), Target: state.RootPath(d.secondConfigTarget), Writable: false},
			})
			if err := reconfigure(state.Stdin, output, secondConfig); err != nil {
				t.Fatalf("Second configuration failed: %v", err)
			}
			if err := utils.DirEntryNamesEqual(state.MountPath(d.dir), []string{"second"}); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestReconfiguration_InodesAreStableForSameUnderlyingFiles(t *testing.T) {
	// inodeOf obtains the inode number of a file.
	inodeOf := func(path string) uint64 {
		fileInfo, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("Failed to get inode number of %s: %v", path, err)
		}
		return fileInfo.Sys().(*syscall.Stat_t).Ino
	}

	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutReader.Close()
	defer stdoutWriter.Close()
	output := bufio.NewScanner(stdoutReader)

	state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr)
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir1"), 0755)
	utils.MustMkdirAll(t, state.RootPath("dir2"), 0755)
	utils.MustMkdirAll(t, state.RootPath("dir3"), 0755)
	utils.MustWriteFile(t, state.RootPath("dir1/file"), 0644, "Hello")
	utils.MustWriteFile(t, state.RootPath("dir2/file"), 0644, "Hello")
	utils.MustWriteFile(t, state.RootPath("dir3/file"), 0644, "Hello")

	wantInodes := make(map[string]uint64)

	firstConfig := jsonConfig([]sandbox.MappingSpec{
		{Mapping: "/dir1", Target: state.RootPath("dir1"), Writable: false},
		{Mapping: "/dir3", Target: state.RootPath("dir3"), Writable: false},
	})
	if err := reconfigure(state.Stdin, output, firstConfig); err != nil {
		t.Fatalf("First configuration failed: %v", err)
	}
	wantInodes["dir1"] = inodeOf(state.MountPath("dir1"))
	wantInodes["dir1/file"] = inodeOf(state.MountPath("dir1/file"))
	wantInodes["dir3"] = inodeOf(state.MountPath("dir3"))
	wantInodes["dir3/file"] = inodeOf(state.MountPath("dir3/file"))

	secondConfig := jsonConfig([]sandbox.MappingSpec{
		{Mapping: "/dir2", Target: state.RootPath("dir2"), Writable: false},
	})
	if err := reconfigure(state.Stdin, output, secondConfig); err != nil {
		t.Fatalf("Failed to replace all mappings with new configuration: %v", err)
	}
	wantInodes["dir2"] = inodeOf(state.MountPath("dir2"))
	wantInodes["dir2/file"] = inodeOf(state.MountPath("dir2/file"))

	if err := reconfigure(state.Stdin, output, firstConfig); err != nil {
		t.Fatalf("Failed to restore all mappings from first configuration: %v", err)
	}

	for _, name := range []string{"dir1", "dir3"} {
		inode := inodeOf(state.MountPath(name))
		// We currently cannot reuse directory nodes because of the internal representation
		// used for them, as any sharing could result in the spurious exposure of in-memory
		// entries that don't exist on disk. Just assert that the user-visible consequences
		// of this remain true.
		if wantInodes[name] == inode {
			t.Errorf("Inode for %s was respected across reconfigurations but it should not have been", name)
		}
	}

	for _, name := range []string{"dir1/file", "dir3/file"} {
		inode := inodeOf(state.MountPath(name))
		if wantInodes[name] != inode {
			t.Errorf("Inode for %s was not respected across reconfigurations: got %d, want %d", name, inode, wantInodes[name])
		}
	}

	for name, inode := range wantInodes {
		if name != "dir2/file" && inode == wantInodes["dir2/file"] {
			t.Errorf("Inode of dir2/file (%d) was reused for some unrelated file %s", inode, name)
		}
	}
}

func TestReconfiguration_WritableNodesAreDifferent(t *testing.T) {
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutReader.Close()
	defer stdoutWriter.Close()
	output := bufio.NewScanner(stdoutReader)

	state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr)
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir1"), 0755)

	config := jsonConfig([]sandbox.MappingSpec{
		{Mapping: "/dir1", Target: state.RootPath("dir1"), Writable: true},
	})
	if err := reconfigure(state.Stdin, output, config); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(state.MountPath("dir1/dir2"), 0755); err != nil {
		t.Errorf("Failed to create entry in writable directory: %v", err)
	}

	config = jsonConfig([]sandbox.MappingSpec{
		{Mapping: "/dir1", Target: state.RootPath("dir1"), Writable: false},
	})
	if err := reconfigure(state.Stdin, output, config); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(state.MountPath("dir1/dir3"), 0755); !os.IsPermission(err) {
		t.Errorf("Writable mapping was not properly downgraded to read-only: got %v; want permission error", err)
	}
}

func TestReconfiguration_FileSystemStillWorksAfterInputEOF(t *testing.T) {
	// grepStderr reads from a pipe connected to stderr looking for the given pattern and writes
	// to the found channel when the pattern is found.  Any contents read from the pipe are
	// dumped to the process' stderr so that they are visible to the user, and so that the child
	// process connected to the pipe does not stall due to a full pipe.
	grepStderr := func(stderr io.Reader, pattern string, found chan<- bool) {
		scanner := bufio.NewScanner(stderr)

		for {
			if !scanner.Scan() {
				if err := scanner.Err(); err != io.EOF && err != io.ErrClosedPipe {
					t.Errorf("Got error while reading from stderr: %v", err)
				}
				break
			}

			fmt.Fprintln(os.Stderr, scanner.Text())

			if utils.MatchesRegexp(pattern, scanner.Text()) {
				found <- true
			}
		}
	}

	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutReader.Close()
	defer stdoutWriter.Close()
	output := bufio.NewScanner(stdoutReader)

	stderrReader, stderrWriter := io.Pipe()
	defer stderrReader.Close()
	defer stderrWriter.Close()

	state := utils.MountSetupWithOutputs(t, stdoutWriter, stderrWriter)
	defer state.TearDown(t)

	gotEOF := make(chan bool)
	go grepStderr(stderrReader, `reached end of input`, gotEOF)

	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)
	config := jsonConfig([]sandbox.MappingSpec{
		{Mapping: "/dir", Target: state.RootPath("dir"), Writable: true},
	})
	if err := reconfigure(state.Stdin, output, config); err != nil {
		t.Fatal(err)
	}

	if err := state.Stdin.Close(); err != nil {
		t.Fatalf("Failed to close stdin: %v", err)
	}
	state.Stdin = nil // Tell state.TearDown that we cleaned up ourselves.
	<-gotEOF

	// sandboxfs stopped listening for reconfiguration requests but the file system should
	// continue to be functional.  Make sure that's the case.
	if err := os.MkdirAll(state.MountPath("dir/still-alive"), 0755); err != nil {
		t.Errorf("Mkdir failed: %v", err)
	}
}

func TestReconfiguration_StreamFileDoesNotExist(t *testing.T) {
	tempDir, err := ioutil.TempDir("", "test")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tempDir)

	nonExistentFile := filepath.Join(tempDir, "non-existent/file")

	testData := []struct {
		name string

		flag       string
		wantStderr string
	}{
		{
			"input",
			"--input=" + nonExistentFile,
			fmt.Sprintf("unable to open file \"%s\" for reading: open %s: no such file or directory", nonExistentFile, nonExistentFile),
		},
		{
			"output",
			"--output=" + nonExistentFile,
			fmt.Sprintf("unable to open file \"%s\" for writing: open %s: no such file or directory", nonExistentFile, nonExistentFile),
		},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			stdout, stderr, err := utils.RunAndWait(1, d.flag, filepath.Join(tempDir, "mnt"))
			if err != nil {
				t.Fatal(err)
			}
			if len(stdout) > 0 {
				t.Errorf("Got %s; want stdout to be empty", stdout)
			}
			if !utils.MatchesRegexp(d.wantStderr, stderr) {
				t.Errorf("Got %s; want stderr to match %s", stderr, d.wantStderr)
			}
		})
	}
}

func TestReconfiguration_InvalidationsRaceWithWrites(t *testing.T) {
	// This is a race-condition test: we attempt to mutate the in-memory nodes of the file
	// system while reconfiguration operations are in-progress to ensure that handling those
	// reconfigurations doesn't collide with the mutations.  Given that this tries to exercise a
	// race condition, a success in this test does not imply that things work correctly, but a
	// failure is a conclusive indication of a real bug.
	//
	// The way we exercise this race is: first, we create a large number of files and expose
	// them through sandboxfs.  We then issue individual Lookup operations on each file (by
	// reading the files by name, *NOT* by doing a ReadDir), which internally must update the
	// contents of the directory known so far.  Concurrently, we hammer the sandboxfs process
	// with reconfiguration requests.  If all goes well, all reads should succeed and sandboxfs
	// should exit cleanly; any other outcome is a failure.

	// createEntries fills the given directory with n files named [0..n-1].
	createEntries := func(dir string, n int) {
		for i := 0; i < n; i++ {
			path := filepath.Join(dir, fmt.Sprintf("%d", i))
			if err := ioutil.WriteFile(path, []byte{}, 0644); err != nil {
				t.Errorf("WriteFile of %s failed: %v", path, err)
			}

			if i%100 == 0 {
				t.Logf("Done creating %d files", i)
			}
		}
	}

	// readEntries reads n files named [0..n-1] from the given directory.
	//
	// As described above, this must not issue a ReadDir operation.  Instead, it must look up
	// the files individually so that sandboxfs receives individual requests at the directory
	// level for each.
	readEntries := func(dir string, n int) {
		for i := 0; i < n; i++ {
			path := filepath.Join(dir, fmt.Sprintf("%d", i))
			if _, err := ioutil.ReadFile(path); err != nil {
				t.Errorf("ReadFile of %s failed: %v", path, err)
			}

			if i%100 == 0 {
				t.Logf("Done looking up and reading %d files", i)
			}
		}
		t.Logf("Done looking up and reading %d files", n)
	}

	// hammerReconfigurations wraps reconfigure in a tight loop to flood the file system with
	// requests to update its configuration.  Requesting a cancellation via the context causes
	// this to terminate, which then notifies the caller by writing to done.
	hammerReconfigurations := func(ctx context.Context, input io.Writer, output *bufio.Scanner, config string, done chan<- bool) {
		for i := 0; ; i++ {
			if i%500 == 0 {
				t.Logf("Reconfiguration number %d", i)
			}

			if err := reconfigure(input, output, config); err != nil {
				t.Fatal(err)
			}

			select {
			case <-ctx.Done():
				done <- true
				return
			default:
				// Just try again immediately.  We want this to be very aggressive,
				// as we are trying to catch very subtle race problems.
			}
		}
	}

	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutReader.Close()
	defer stdoutWriter.Close()
	output := bufio.NewScanner(stdoutReader)

	state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr)
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)
	config := jsonConfig([]sandbox.MappingSpec{
		{Mapping: "/dir", Target: state.RootPath("dir"), Writable: false},
	})

	nEntries := 2000
	utils.MustMkdirAll(t, state.RootPath("dir/subdir"), 0755)
	createEntries(state.RootPath("dir/subdir"), nEntries)

	if err := reconfigure(state.Stdin, output, config); err != nil {
		t.Fatal(err)
	}

	done := make(chan bool)
	ctx, cancel := context.WithCancel(context.Background())
	go hammerReconfigurations(ctx, state.Stdin, output, config, done)
	readEntries(state.MountPath("dir/subdir"), nEntries)
	cancel()
	<-done
}

// TODO(jmmv): Need to have tests for when the configuration is invalid (malformed JSON,
// inconsistent mappings, etc.).  No need for these to be very detailed given that the validations
// are already tested in "static" mode, but we must ensure that such validation paths are also
// exercised in dynamic mode when the configuration is processed.
