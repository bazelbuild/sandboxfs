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
)

// tryReconfigure pushes a new configuration to the sandboxfs process and returns the response from
// the process.
func tryReconfigure(input io.Writer, output *bufio.Scanner, root string, config string) (string, error) {
	config = strings.Replace(config, "%ROOT%", root, -1) + "\n\n"
	n, err := io.WriteString(input, config)
	if err != nil {
		return "", fmt.Errorf("failed to send new configuration to sandboxfs: %v", err)
	}
	if n != len(config) {
		return "", fmt.Errorf("failed to send full configuration to sandboxfs: got %d bytes, want %d bytes", n, len(config))
	}

	if !output.Scan() {
		if err := output.Err(); err != nil {
			return "", fmt.Errorf("failed to read from sandboxfs's output: %v", err)
		}
		return "", fmt.Errorf("no data available in sandboxfs's output")
	}
	return output.Text(), nil
}

// reconfigure pushes a new configuration to the sandboxfs process and waits for acknowledgement.
func reconfigure(input io.Writer, output *bufio.Scanner, root string, config string) error {
	message, err := tryReconfigure(input, output, root, config)
	if err != nil {
		return err
	}
	doneMarker := "Done"
	if message != doneMarker {
		return fmt.Errorf("sandboxfs did not ack configuration: got %s, want %s", output.Text(), doneMarker)
	}
	return nil
}

func TestReconfiguration_Streams(t *testing.T) {
	reconfigureAndCheck := func(t *testing.T, state *utils.MountState, input io.Writer, outputReader io.Reader) {
		output := bufio.NewScanner(outputReader)

		utils.MustMkdirAll(t, state.RootPath("a/b"), 0755)
		config := `[{"Map": {"Mapping": "/ro", "Target": "%ROOT%/a", "Writable": false}}]`
		if err := reconfigure(input, output, state.RootPath(), config); err != nil {
			t.Fatal(err)
		}

		if _, err := os.Lstat(state.MountPath("ro/b")); err != nil {
			t.Errorf("Cannot stat a/b in mount point; reconfiguration failed? Got %v", err)
		}
	}

	// TODO(jmmv): Consider dropping stdin/stdout support as defaults.  This is quite an
	// artificial construct and makes our testing quite complex.
	t.Run("Default", func(t *testing.T) {
		stdoutReader, stdoutWriter := io.Pipe()
		defer stdoutReader.Close() // Just in case the test fails half-way through.
		defer stdoutWriter.Close() // Just in case the test fails half-way through.

		state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr)
		defer state.TearDown(t)
		reconfigureAndCheck(t, state, state.Stdin, stdoutReader)
	})

	t.Run("Explicit", func(t *testing.T) {
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

		reconfigureAndCheck(t, state, input, output)
	})
}

func TestReconfiguration_Steps(t *testing.T) {
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutReader.Close() // Just in case the test fails half-way through.
	defer stdoutWriter.Close() // Just in case the test fails half-way through.
	output := bufio.NewScanner(stdoutReader)

	state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr, "-mapping=ro:/:%ROOT%", "-mapping=rw:/initial:%ROOT%/initial")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("some/read-only-dir"), 0755)
	utils.MustMkdirAll(t, state.RootPath("some/read-write-dir"), 0755)
	config := `[
		{"Map": {"Mapping": "/ro", "Target": "%ROOT%/some/read-only-dir", "Writable": false}},
		{"Map": {"Mapping": "/ro/rw", "Target": "%ROOT%/some/read-write-dir", "Writable": true}},
		{"Map": {"Mapping": "/nested/dup", "Target": "%ROOT%/some/read-only-dir", "Writable": false}}
	]`
	if err := reconfigure(state.Stdin, output, state.RootPath(), config); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(state.MountPath("initial")); err != nil {
		t.Errorf("Initial mapping seems to be gone; got %v", err)
	}
	if err := os.MkdirAll(state.MountPath("ro/hello"), 0755); err == nil {
		t.Errorf("Mkdir succeeded in read-only mapping")
	}
	if _, err := os.Lstat(state.RootPath("some/read-only-dir/hello")); err == nil {
		t.Errorf("Mkdir through read-only mapping propagated to root")
	}
	if err := os.MkdirAll(state.MountPath("ro/rw/hello2"), 0755); err != nil {
		t.Errorf("Mkdir failed in nested read-write mapping: %v", err)
	}
	if _, err := os.Lstat(state.RootPath("some/read-write-dir/hello2")); err != nil {
		t.Errorf("Mkdir through read-write didn't propagate to root; got %v", err)
	}
	if err := os.MkdirAll(state.MountPath("a"), 0755); err == nil {
		t.Errorf("Mkdir succeeded in read-only root mapping")
	}

	config = `[
		{"Unmap": "/ro"},
		{"Map": {"Mapping": "/rw/dir", "Target": "%ROOT%/some/read-write-dir", "Writable": true}}
	]`
	if err := reconfigure(state.Stdin, output, state.RootPath(), config); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(state.MountPath("initial")); err != nil {
		t.Errorf("Initial mapping seems to be gone; got %v", err)
	}
	if _, err := os.Lstat(state.MountPath("nested/dup")); err != nil {
		t.Errorf("Previously-mapped /nested/dup seems to be gone; got %v", err)
	}
	if _, err := os.Lstat(state.MountPath("ro")); err == nil {
		t.Errorf("Unmapped /ro is not gone")
	}
	if err := os.MkdirAll(state.MountPath("rw/dir/hello"), 0755); err != nil {
		t.Errorf("Mkdir failed in read-write mapping: %v", err)
	}
}

func TestReconfiguration_Unmap(t *testing.T) {
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutReader.Close() // Just in case the test fails half-way through.
	defer stdoutWriter.Close() // Just in case the test fails half-way through.
	output := bufio.NewScanner(stdoutReader)

	state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr, "-mapping=ro:/:%ROOT%", "-mapping=ro:/root-mapping:%ROOT%/foo", "-mapping=ro:/nested/mapping:%ROOT%/foo", "-mapping=ro:/deep/a/b/c/d:%ROOT%/foo")
	defer state.TearDown(t)

	config := `[
		{"Unmap": "/root-mapping"},
		{"Unmap": "/nested/mapping"},
		{"Unmap": "/deep/a"}
	]`
	if err := reconfigure(state.Stdin, output, state.RootPath(), config); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/root-mapping", "/nested/mapping", "/deep/a/b/c/d", "/deep/a/b/c", "/deep/a/b"} {
		if _, err := os.Lstat(state.MountPath(path)); err == nil {
			t.Errorf("Unmapped %s is not gone", path)
		}
	}
	for _, path := range []string{"/nested", "/deep"} {
		if _, err := os.Lstat(state.MountPath(path)); err != nil {
			t.Errorf("Mapping %s should have been left untouched but was removed; got %v", path, err)
		}
	}

	config = `[{"Unmap": "/nested"}]`
	if err := reconfigure(state.Stdin, output, state.RootPath(), config); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(state.MountPath("nested")); err == nil {
		t.Errorf("Unmapped nested mapping is not gone")
	}
}

func TestReconfiguration_RemapInvalidatesCache(t *testing.T) {
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutReader.Close() // Just in case the test fails half-way through.
	defer stdoutWriter.Close() // Just in case the test fails half-way through.
	output := bufio.NewScanner(stdoutReader)

	state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr, "-mapping=ro:/:%ROOT%")
	defer state.TearDown(t)

	checkMountPoint := func(wantExist string, wantNotExist string, wantFileContents string, wantLink string) {
		for _, subdir := range []string{"", "/z"} {
			if _, err := os.Lstat(state.MountPath(subdir, wantExist)); err != nil {
				t.Errorf("%s not present: %v", filepath.Join(subdir, wantExist), err)
			}
			if _, err := os.Lstat(state.MountPath(subdir, wantNotExist)); wantNotExist != "" && err == nil {
				t.Errorf("%s present but should not have been", filepath.Join(subdir, wantNotExist))
			}
			if err := utils.FileEquals(state.MountPath(subdir, "file"), wantFileContents); err != nil {
				t.Errorf("%s does not match expected contents: %v", filepath.Join(subdir, "file"), err)
			}
			if link, err := os.Readlink(state.MountPath(subdir, "symlink")); err != nil {
				t.Errorf("%s not present: %v", filepath.Join(subdir, "symlink"), err)
			} else {
				if link != wantLink {
					t.Errorf("%s contents are invalid: got %s, want %s", filepath.Join(subdir, "symlink"), link, wantLink)
				}
			}
		}
	}

	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)
	utils.MustMkdirAll(t, state.RootPath("dir/original-entry"), 0755)
	utils.MustWriteFile(t, state.RootPath("file"), 0644, "original file contents")
	utils.MustSymlink(t, "/non-existent", state.RootPath("symlink"))

	config := `[
		{"Map": {"Mapping": "/dir", "Target": "%ROOT%/dir", "Writable": false}},
		{"Map": {"Mapping": "/file", "Target": "%ROOT%/file", "Writable": false}},
		{"Map": {"Mapping": "/symlink", "Target": "%ROOT%/symlink", "Writable": false}},
		{"Map": {"Mapping": "/z/dir", "Target": "%ROOT%/dir", "Writable": false}},
		{"Map": {"Mapping": "/z/file", "Target": "%ROOT%/file", "Writable": false}},
		{"Map": {"Mapping": "/z/symlink", "Target": "%ROOT%/symlink", "Writable": false}}
	]`
	if err := reconfigure(state.Stdin, output, state.RootPath(), config); err != nil {
		t.Fatal(err)
	}
	checkMountPoint("dir/original-entry", "", "original file contents", "/non-existent")

	for _, file := range []string{"dir/original-entry", "file", "symlink"} {
		if err := os.Remove(state.RootPath(file)); err != nil {
			t.Fatalf("Failed to remove %s while recreating files: %v", file, err)
		}
	}
	utils.MustMkdirAll(t, state.RootPath("dir/new-entry"), 0755)
	utils.MustWriteFile(t, state.RootPath("file"), 0644, "new file contents")
	utils.MustSymlink(t, "/non-existent-other", state.RootPath("symlink"))

	config = `[
		{"Unmap": "/dir"},
		{"Map": {"Mapping": "/dir", "Target": "%ROOT%/dir", "Writable": false}},
		{"Unmap": "/file"},
		{"Map": {"Mapping": "/file", "Target": "%ROOT%/file", "Writable": false}},
		{"Unmap": "/symlink"},
		{"Map": {"Mapping": "/symlink", "Target": "%ROOT%/symlink", "Writable": false}},
		{"Unmap": "/z/dir"},
		{"Map": {"Mapping": "/z/dir", "Target": "%ROOT%/dir", "Writable": false}},
		{"Unmap": "/z/file"},
		{"Map": {"Mapping": "/z/file", "Target": "%ROOT%/file", "Writable": false}},
		{"Unmap": "/z/symlink"},
		{"Map": {"Mapping": "/z/symlink", "Target": "%ROOT%/symlink", "Writable": false}}
	]`
	if err := reconfigure(state.Stdin, output, state.RootPath(), config); err != nil {
		t.Fatal(err)
	}
	checkMountPoint("dir/new-entry", "dir/original-entry", "new file contents", "/non-existent-other")
}

func TestReconfiguration_Errors(t *testing.T) {
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdoutReader.Close() // Just in case the test fails half-way through.
	defer stdoutWriter.Close() // Just in case the test fails half-way through.
	output := bufio.NewScanner(stdoutReader)

	state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr, "-mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("subdir"), 0755)
	utils.MustWriteFile(t, state.RootPath("file"), 0644, "")

	testData := []struct {
		name string

		config    string
		wantError string
	}{
		{
			"InvalidSyntax",
			`this is not in the correct format`,
			"unable to parse json",
		},
		{
			"InvalidMapping",
			`[{"Map": {"Mapping": "foo/../.", "Target": "%ROOT%/subdir", "Writable": false}}]`,
			"invalid mapping foo/../.: empty path",
		},
		{
			"EmptyStep",
			`[{}]`,
			"neither Map nor Unmap were defined",
		},
		{
			"MapAndUnmapInSameStep",
			`[{"Map": {"Mapping": "/foo", "Target": "%ROOT%", "Writable": false}, "Unmap": "/bar"}]`,
			"Map and Unmap are exclusive",
		},
		{
			"RemapRoot",
			`[{"Map": {"Mapping": "/", "Target": "%ROOT%/subdir", "Writable": false}}]`,
			"root.*not more than once",
		},
		{
			"MapTwice",
			`[{"Map": {"Mapping": "/foo", "Target": "%ROOT%/subdir", "Writable": false}}, {"Map": {"Mapping": "/foo", "Target": "%ROOT%/file", "Writable": false}}]`,
			"already mapped",
		},
		{
			"UnmapRoot",
			`[{"Unmap": "/"}]`,
			"cannot unmap root",
		},
		{
			"UnmapRealUnmappedFile",
			`[{"Unmap": "/file"}]`,
			"not a mapping",
		},
		{
			"UnmapMissingEntryInMapping",
			`[{"Map": {"Mapping": "/subdir", "Target": "%ROOT%/subdir", "Writable": false}}, {"Unmap": "/subdir/foo"}]`,
			"leaf foo not mapped",
		},
		{
			"UnmapMissingEntryInRealUnmappedDirectory",
			`[{"Unmap": "/subdir/foo"}]`,
			"leaf foo not mapped",
		},
		{
			"UnmapPathWithMissingComponents",
			`[{"Unmap": "/missing/long/path"}]`,
			"intermediate components not mapped",
		},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			message, err := tryReconfigure(state.Stdin, output, state.RootPath(), d.config)
			if err != nil {
				t.Fatalf("want reconfiguration of / to fail; got success")
			}
			if !utils.MatchesRegexp(d.wantError, message) {
				t.Errorf("want reconfiguration to respond with %s; got %s", d.wantError, message)
			}
			if _, err := os.Lstat(state.MountPath("file")); err != nil {
				t.Errorf("want file to still exist after failed reconfiguration; got %v", err)
			}
		})
	}
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

		firstConfig := `[{"Map": {"Mapping": "/first", "Target": "%ROOT%/first", "Writable": false}}]`
		if err := reconfigure(state.Stdin, output, state.RootPath(), firstConfig); err != nil {
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

			firstConfig := fmt.Sprintf(`[{"Map": {"Mapping": "%s", "Target": "%s", "Writable": false}}]`, filepath.Join(d.dir, "first"), state.RootPath(d.firstConfigTarget))
			if err := reconfigure(state.Stdin, output, state.RootPath(), firstConfig); err != nil {
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

			secondConfig := fmt.Sprintf(`[
				{"Unmap": "%s"},
				{"Map": {"Mapping": "%s", "Target": "%s", "Writable": false}}
			]`, filepath.Join(d.dir, "first"), filepath.Join(d.dir, "second"), state.RootPath(d.secondConfigTarget))
			if err := reconfigure(state.Stdin, output, state.RootPath(), secondConfig); err != nil {
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

	firstConfig := `[
		{"Map": {"Mapping": "/dir1", "Target": "%ROOT%/dir1", "Writable": false}},
		{"Map": {"Mapping": "/dir3", "Target": "%ROOT%/dir3", "Writable": false}}
	]`
	if err := reconfigure(state.Stdin, output, state.RootPath(), firstConfig); err != nil {
		t.Fatalf("First configuration failed: %v", err)
	}
	wantInodes["dir1"] = inodeOf(state.MountPath("dir1"))
	wantInodes["dir1/file"] = inodeOf(state.MountPath("dir1/file"))
	wantInodes["dir3"] = inodeOf(state.MountPath("dir3"))
	wantInodes["dir3/file"] = inodeOf(state.MountPath("dir3/file"))

	secondConfig := `[
		{"Unmap": "/dir1"},
		{"Unmap": "/dir3"},
		{"Map": {"Mapping": "/dir2", "Target": "%ROOT%/dir2", "Writable": false}}
	]`
	if err := reconfigure(state.Stdin, output, state.RootPath(), secondConfig); err != nil {
		t.Fatalf("Failed to replace all mappings with new configuration: %v", err)
	}
	wantInodes["dir2"] = inodeOf(state.MountPath("dir2"))
	wantInodes["dir2/file"] = inodeOf(state.MountPath("dir2/file"))

	if err := reconfigure(state.Stdin, output, state.RootPath(), firstConfig); err != nil {
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

	config := `[{"Map": {"Mapping": "/dir1", "Target": "%ROOT%/dir1", "Writable": true}}]`
	if err := reconfigure(state.Stdin, output, state.RootPath(), config); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(state.MountPath("dir1/dir2"), 0755); err != nil {
		t.Errorf("Failed to create entry in writable directory: %v", err)
	}

	config = `[
		{"Unmap": "/dir1"},
		{"Map": {"Mapping": "/dir1", "Target": "%ROOT%/dir1", "Writable": false}}
	]`
	if err := reconfigure(state.Stdin, output, state.RootPath(), config); err != nil {
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
	config := `[{"Map": {"Mapping": "/dir", "Target": "%ROOT%/dir", "Writable": true}}]`
	if err := reconfigure(state.Stdin, output, state.RootPath(), config); err != nil {
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
