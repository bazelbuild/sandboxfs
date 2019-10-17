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
	"encoding/json"
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

const invalidationsWork = false

// mapping represents a mapping entry in the reconfiguration protocol.
type mapping struct {
	Path           string `json:"path"`
	UnderlyingPath string `json:"underlying_path"`
	Writable       bool   `json:"writable"`
}

// mapStep represents a map operation in the reconfiguration protocol.
type mapStep struct {
	Root     string    `json:"root"`
	Mappings []mapping `json:"mappings"`
}

// unmapStep represents an unmap operation in the reconfiguration protocol.
type unmapStep struct {
	Root     string   `json:"root"`
	Mappings []string `json:"mappings"`
}

// step represents a map or unmap operation in the reconfiguration protocol. Only one of these
// fields can be used at a time.
type step struct {
	Map   *mapStep   `json:"Map,omitempty"`
	Unmap *unmapStep `json:"Unmap,omitempty"`
}

// request represents a single reconfiguration request.
type request []step

// response represents the result of a reconfiguration request.
type response struct {
	Error *string `json:"error,omitempty"`
}

// makeMapStep is a convenience function to instantiate a single map step.
func makeMapStep(root string, mapping1 mapping, mappingN ...mapping) step {
	return step{Map: &mapStep{Root: root, Mappings: append([]mapping{mapping1}, mappingN...)}}
}

// makeUnmapStep is a convenience function to instantiate a single unmap step.
func makeUnmapStep(root string, mapping1 string, mappingN ...string) step {
	return step{Unmap: &unmapStep{Root: root, Mappings: append([]string{mapping1}, mappingN...)}}
}

// tryRawReconfigure pushes a new configuration to the sandboxfs process and waits for
// acknowledgement. The reconfiguration request is provided as a string, which may be invalid (to
// verify error cases). Returns the error message from the server, which might be nil.
func tryRawReconfigure(input io.Writer, output io.Reader, root string, config string) (*string, error) {
	config = strings.Replace(string(config), "%ROOT%", root, -1) + "\n"
	n, err := io.WriteString(input, config)
	if err != nil {
		return nil, fmt.Errorf("failed to send new configuration to sandboxfs: %v", err)
	}
	if n != len(config) {
		return nil, fmt.Errorf("failed to send full configuration to sandboxfs: got %d bytes, want %d bytes", n, len(config))
	}

	decoder := json.NewDecoder(output)
	r := response{}
	if err := decoder.Decode(&r); err != nil {
		return nil, fmt.Errorf("failed to read from sandboxfs's output: %v", err)
	}
	return r.Error, nil
}

// tryReconfigure pushes a new configuration to the sandboxfs process and waits for
// acknowledgement. The reconfiguration request is provided as a sequence of objects, which is
// assumed to be valid (except for semantical errors). Returns the error message from the server,
// which might be nil.
func tryReconfigure(input io.Writer, output io.Reader, root string, config request) (*string, error) {
	configBytes, err := json.Marshal(config)
	if err != nil {
		panic(fmt.Sprintf("Bad configuration request in test: %v", err))
	}
	return tryRawReconfigure(input, output, root, string(configBytes))
}

// reconfigure pushes a new configuration to the sandboxfs process and waits for acknowledgement.
func reconfigure(input io.Writer, output io.Reader, root string, config request) error {
	message, err := tryReconfigure(input, output, root, config)
	if err != nil {
		return err
	}
	if message != nil {
		return fmt.Errorf("sandboxfs did not ack configuration: %s", *message)
	}
	return nil
}

// existsViaReaddir checks if a directory entry exists within a directory by scanning the contents
// of the directory itself (i.e. via readdir), not by attempting a direct lookup on the entry.
func existsViaReaddir(dir string, name string) (bool, error) {
	dirents, err := ioutil.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, dirent := range dirents {
		if dirent.Name() == name {
			return true, nil
		}
	}
	return false, nil
}

// errorIfNotUnmapped fails the calling test case if the given directory entry, which is expected to
// not be mapped any longer, still exists.
func errorIfNotUnmapped(t *testing.T, dir string, name string) {
	if !invalidationsWork {
		// The FUSE library we use from Rust currently lacks support for kernel cache
		// invalidations.  As a result, sandboxfs cannot make unmapped entries disappear
		// right away (and setting a lower TTL for the file system does not help because
		// OSXFUSE does nothing with the entries' TTL).  We know that this is suboptimal,
		// but instead of making the tests fail, make sure sandboxfs mostly works by
		// checking that it really did unmap the entry (which we can confirm by looking
		// at readdir output).
		exists, err := existsViaReaddir(dir, name)
		if err != nil {
			t.Errorf("Failed to read contents of %s: %v", dir, err)
		} else if exists {
			t.Errorf("Unmapped %s is not gone from %s", name, dir)
		}
	} else {
		path := filepath.Join(dir, name)
		_, err := os.Lstat(path)
		if err != nil && !os.IsNotExist(err) {
			t.Errorf("Failed to stat %s: %v", path, err)
		} else if err == nil {
			t.Errorf("Unmapped %s is not gone from %s", name, dir)
		}
	}
}

func TestReconfiguration_Streams(t *testing.T) {
	reconfigureAndCheck := func(t *testing.T, state *utils.MountState, input io.Writer, output io.Reader) {
		utils.MustMkdirAll(t, state.RootPath("a/b"), 0755)
		config := request{
			makeMapStep("/", mapping{Path: "/ro", UnderlyingPath: "%ROOT%/a", Writable: false}),
		}
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
		state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr)
		defer stdoutReader.Close() // Just in case the test fails half-way through.
		defer state.TearDown(t)
		defer stdoutWriter.Close() // Just in case the test fails half-way through.
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

		state := utils.MountSetupWithOutputs(t, nil, os.Stderr, "--input="+inFifo, "--output="+outFifo)
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
	state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr, "--mapping=ro:/:%ROOT%", "--mapping=rw:/initial:%ROOT%/initial")
	defer stdoutReader.Close() // Just in case the test fails half-way through.
	defer state.TearDown(t)
	defer stdoutWriter.Close() // Just in case the test fails half-way through.

	utils.MustMkdirAll(t, state.RootPath("some/read-only-dir"), 0755)
	utils.MustMkdirAll(t, state.RootPath("some/read-write-dir"), 0755)
	config := request{
		makeMapStep("/", mapping{Path: "/ro", UnderlyingPath: "%ROOT%/some/read-only-dir", Writable: false}),
		makeMapStep("/", mapping{Path: "/ro/rw", UnderlyingPath: "%ROOT%/some/read-write-dir", Writable: true}),
		makeMapStep("/", mapping{Path: "/nested/dup", UnderlyingPath: "%ROOT%/some/read-only-dir", Writable: false}),
	}
	if err := reconfigure(state.Stdin, stdoutReader, state.RootPath(), config); err != nil {
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

	config = request{
		makeUnmapStep("/", "/ro"),
		makeMapStep("/", mapping{Path: "/rw/dir", UnderlyingPath: "%ROOT%/some/read-write-dir", Writable: true}),
	}
	if err := reconfigure(state.Stdin, stdoutReader, state.RootPath(), config); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(state.MountPath("initial")); err != nil {
		t.Errorf("Initial mapping seems to be gone; got %v", err)
	}
	if _, err := os.Lstat(state.MountPath("nested/dup")); err != nil {
		t.Errorf("Previously-mapped /nested/dup seems to be gone; got %v", err)
	}
	errorIfNotUnmapped(t, state.MountPath(), "ro")
	if err := os.MkdirAll(state.MountPath("rw/dir/hello"), 0755); err != nil {
		t.Errorf("Mkdir failed in read-write mapping: %v", err)
	}
}

func TestReconfiguration_Unmap(t *testing.T) {
	stdoutReader, stdoutWriter := io.Pipe()
	state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr, "--mapping=ro:/:%ROOT%", "--mapping=ro:/root-mapping:%ROOT%/foo", "--mapping=ro:/nested/mapping:%ROOT%/foo", "--mapping=ro:/deep/a/b/c/d:%ROOT%/foo")
	defer stdoutReader.Close() // Just in case the test fails half-way through.
	defer state.TearDown(t)
	defer stdoutWriter.Close() // Just in case the test fails half-way through.

	config := request{
		makeUnmapStep("/", "/root-mapping"),
		makeUnmapStep("/", "/nested/mapping"),
		makeUnmapStep("/", "/deep/a"),
	}
	if err := reconfigure(state.Stdin, stdoutReader, state.RootPath(), config); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/root-mapping", "/nested/mapping", "/deep/a/b/c/d", "/deep/a/b/c", "/deep/a/b"} {
		errorIfNotUnmapped(t, state.MountPath(), path)
	}
	for _, path := range []string{"/nested", "/deep"} {
		if _, err := os.Lstat(state.MountPath(path)); err != nil {
			t.Errorf("Mapping %s should have been left untouched but was removed; got %v", path, err)
		}
	}

	config = request{makeUnmapStep("/", "/nested")}
	if err := reconfigure(state.Stdin, stdoutReader, state.RootPath(), config); err != nil {
		t.Fatal(err)
	}
	errorIfNotUnmapped(t, state.MountPath(), "nested")
}

func TestReconfiguration_RemapInvalidatesCache(t *testing.T) {
	if !invalidationsWork {
		t.Skipf("Kernel cache invalidations are not currently supported")
	}

	stdoutReader, stdoutWriter := io.Pipe()
	state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr, "--mapping=ro:/:%ROOT%")
	defer stdoutReader.Close() // Just in case the test fails half-way through.
	defer state.TearDown(t)
	defer stdoutWriter.Close() // Just in case the test fails half-way through.

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

	config := request{
		makeMapStep("/", mapping{Path: "/dir", UnderlyingPath: "%ROOT%/dir", Writable: false}),
		makeMapStep("/", mapping{Path: "/file", UnderlyingPath: "%ROOT%/file", Writable: false}),
		makeMapStep("/", mapping{Path: "/symlink", UnderlyingPath: "%ROOT%/symlink", Writable: false}),
		makeMapStep("/", mapping{Path: "/z/dir", UnderlyingPath: "%ROOT%/dir", Writable: false}),
		makeMapStep("/", mapping{Path: "/z/file", UnderlyingPath: "%ROOT%/file", Writable: false}),
		makeMapStep("/", mapping{Path: "/z/symlink", UnderlyingPath: "%ROOT%/symlink", Writable: false}),
	}
	if err := reconfigure(state.Stdin, stdoutReader, state.RootPath(), config); err != nil {
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

	config = request{
		makeUnmapStep("/", "/dir"),
		makeMapStep("/", mapping{Path: "/dir", UnderlyingPath: "%ROOT%/dir", Writable: false}),
		makeUnmapStep("/", "/file"),
		makeMapStep("/", mapping{Path: "/file", UnderlyingPath: "%ROOT%/file", Writable: false}),
		makeUnmapStep("/", "/symlink"),
		makeMapStep("/", mapping{Path: "/symlink", UnderlyingPath: "%ROOT%/symlink", Writable: false}),
		makeUnmapStep("/", "/z/dir"),
		makeMapStep("/", mapping{Path: "/z/dir", UnderlyingPath: "%ROOT%/dir", Writable: false}),
		makeUnmapStep("/", "/z/file"),
		makeMapStep("/", mapping{Path: "/z/file", UnderlyingPath: "%ROOT%/file", Writable: false}),
		makeUnmapStep("/", "/z/symlink"),
		makeMapStep("/", mapping{Path: "/z/symlink", UnderlyingPath: "%ROOT%/symlink", Writable: false}),
	}
	if err := reconfigure(state.Stdin, stdoutReader, state.RootPath(), config); err != nil {
		t.Fatal(err)
	}
	checkMountPoint("dir/new-entry", "dir/original-entry", "new file contents", "/non-existent-other")
}

func TestReconfiguration_RecoverableErrors(t *testing.T) {
	stdoutReader, stdoutWriter := io.Pipe()
	state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr, "--mapping=rw:/:%ROOT%")
	defer stdoutReader.Close() // Just in case the test fails half-way through.
	defer state.TearDown(t)
	defer stdoutWriter.Close() // Just in case the test fails half-way through.

	checkBadConfig := func(t *testing.T, config request, wantError string) {
		t.Helper()
		message, err := tryReconfigure(state.Stdin, stdoutReader, state.RootPath(), config)
		if err != nil {
			t.Fatalf("want reconfiguration of / to fail; got success")
		}
		if message == nil {
			t.Errorf("want reconfiguration to respond with %s; got OK", wantError)
		} else if !utils.MatchesRegexp(wantError, *message) {
			t.Errorf("want reconfiguration to respond with %s; got %s", wantError, *message)
		}
		if _, err := os.Lstat(state.MountPath("file")); err != nil {
			t.Errorf("want file to still exist after failed reconfiguration; got %v", err)
		}
	}

	utils.MustMkdirAll(t, state.RootPath("subdir"), 0755)
	utils.MustWriteFile(t, state.RootPath("file"), 0644, "")

	testData := []struct {
		name string

		config    request
		wantError string
	}{
		{
			"InvalidMapping",
			request{
				makeMapStep("/", mapping{Path: "foo/../.", UnderlyingPath: "%ROOT%/subdir", Writable: false}),
			},
			"path.*not absolute",
		},
		{
			"RemapRoot",
			request{
				makeMapStep("/", mapping{Path: "/", UnderlyingPath: "%ROOT%/subdir", Writable: false}),
			},
			"Root can be mapped at most once",
		},
		{
			"MapTwice",
			request{
				makeMapStep("/", mapping{Path: "/foo", UnderlyingPath: "%ROOT%/subdir", Writable: false}),
				makeMapStep("/", mapping{Path: "/foo", UnderlyingPath: "%ROOT%/file", Writable: false}),
			},
			"Already mapped",
		},
		{
			"UnmapRoot",
			request{
				makeUnmapStep("/", "/"),
			},
			"Root cannot be unmapped",
		},
		{
			"UnmapMissingEntryInMapping",
			request{
				makeMapStep("/", mapping{Path: "/subdir", UnderlyingPath: "%ROOT%/subdir", Writable: false}),
				makeUnmapStep("/", "/subdir/foo"),
			},
			"Unknown entry",
		},
		{
			"UnmapMissingEntryInRealUnmappedDirectory",
			request{
				makeUnmapStep("/", "/subdir/foo"),
			},
			"Unknown entry",
		},
		{
			"UnmapPathWithMissingComponents",
			request{
				makeUnmapStep("/", "/missing/long/path"),
			},
			"Unknown component in entry",
		},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			checkBadConfig(t, d.config, d.wantError)
		})
	}

	t.Run("UnmapRealUnmappedPath", func(t *testing.T) {
		utils.MustMkdirAll(t, state.RootPath("subdir/inner-subdir"), 0755)
		utils.MustWriteFile(t, state.RootPath("subdir/inner-subdir/inner-file"), 0644, "")

		if err := reconfigure(state.Stdin, stdoutReader, state.RootPath(), request{makeMapStep("/", mapping{Path: "/subdir2", UnderlyingPath: "%ROOT%/subdir", Writable: false})}); err != nil {
			t.Fatalf("Failed to map /subdir2: %v", err)
		}

		// Poke the file we just created through the mount point so that sandboxfs sees it
		// and becomes part of any in-memory structures.
		if _, err := os.Lstat(state.MountPath("subdir2/inner-subdir/inner-file")); err != nil {
			t.Fatalf("Failed to stat just-created file subdir2/inner-subdir/inner-file: %v", err)
		}

		checkBadConfig(t, request{makeUnmapStep("/", "/subdir2/inner-subdir/inner-file")}, "inner-file.*is not a mapping")
	})
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
		state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr, "--mapping=ro:/:%ROOT%")
		defer stdoutReader.Close()
		// state.TearDown and stdoutWriter.Close not deferred here because we want to
		// explicitly control for any possible error they may report and abort the whole
		// test early in that case.

		utils.MustWriteFile(t, state.RootPath("first"), 0644, "First")

		firstConfig := request{makeMapStep("/", mapping{Path: "/first", UnderlyingPath: "%ROOT%/first", Writable: false})}
		if err := reconfigure(state.Stdin, stdoutReader, state.RootPath(), firstConfig); err != nil {
			stdoutWriter.Close()
			state.TearDown(t)
			return err
		}
		stdoutWriter.Close()
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
			state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr)
			defer stdoutReader.Close()
			defer state.TearDown(t)
			defer stdoutWriter.Close()

			utils.MustMkdirAll(t, state.RootPath("dir1"), 0755)
			utils.MustWriteFile(t, state.RootPath("dir1/first"), 0644, "First")
			utils.MustMkdirAll(t, state.RootPath("dir2"), 0755)
			utils.MustWriteFile(t, state.RootPath("dir2/second"), 0644, "Second")

			firstConfig := request{makeMapStep("/", mapping{Path: filepath.Join(d.dir, "first"), UnderlyingPath: state.RootPath(d.firstConfigTarget), Writable: false})}
			if err := reconfigure(state.Stdin, stdoutReader, state.RootPath(), firstConfig); err != nil {
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

			secondConfig := request{
				makeUnmapStep("/", filepath.Join(d.dir, "first")),
				makeMapStep("/", mapping{Path: filepath.Join(d.dir, "second"), UnderlyingPath: state.RootPath(d.secondConfigTarget), Writable: false}),
			}
			if err := reconfigure(state.Stdin, stdoutReader, state.RootPath(), secondConfig); err != nil {
				t.Fatalf("Second configuration failed: %v", err)
			}
			if err := utils.DirEntryNamesEqual(state.MountPath(d.dir), []string{"second"}); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestReconfiguration_InodesAreStableForSameUnderlyingFiles(t *testing.T) {
	if !invalidationsWork {
		t.Skipf("Kernel cache invalidations are not currently supported")
	}

	// inodeOf obtains the inode number of a file.
	inodeOf := func(path string) uint64 {
		fileInfo, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("Failed to get inode number of %s: %v", path, err)
		}
		return fileInfo.Sys().(*syscall.Stat_t).Ino
	}

	stdoutReader, stdoutWriter := io.Pipe()
	state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr)
	defer stdoutReader.Close()
	defer state.TearDown(t)
	defer stdoutWriter.Close()

	utils.MustMkdirAll(t, state.RootPath("dir1"), 0755)
	utils.MustMkdirAll(t, state.RootPath("dir2"), 0755)
	utils.MustMkdirAll(t, state.RootPath("dir3"), 0755)
	utils.MustWriteFile(t, state.RootPath("dir1/file"), 0644, "Hello")
	utils.MustWriteFile(t, state.RootPath("dir2/file"), 0644, "Hello")
	utils.MustWriteFile(t, state.RootPath("dir3/file"), 0644, "Hello")

	wantInodes := make(map[string]uint64)

	firstConfig := request{
		makeMapStep("/", mapping{Path: "/dir1", UnderlyingPath: "%ROOT%/dir1", Writable: false}),
		makeMapStep("/", mapping{Path: "/dir3", UnderlyingPath: "%ROOT%/dir3", Writable: false}),
	}
	if err := reconfigure(state.Stdin, stdoutReader, state.RootPath(), firstConfig); err != nil {
		t.Fatalf("First configuration failed: %v", err)
	}
	wantInodes["dir1"] = inodeOf(state.MountPath("dir1"))
	wantInodes["dir1/file"] = inodeOf(state.MountPath("dir1/file"))
	wantInodes["dir3"] = inodeOf(state.MountPath("dir3"))
	wantInodes["dir3/file"] = inodeOf(state.MountPath("dir3/file"))

	secondConfig := request{
		makeUnmapStep("/", "/dir1"),
		makeUnmapStep("/", "/dir3"),
		makeMapStep("/", mapping{Path: "/dir2", UnderlyingPath: "%ROOT%/dir2", Writable: false}),
	}
	if err := reconfigure(state.Stdin, stdoutReader, state.RootPath(), secondConfig); err != nil {
		t.Fatalf("Failed to replace all mappings with new configuration: %v", err)
	}
	wantInodes["dir2"] = inodeOf(state.MountPath("dir2"))
	wantInodes["dir2/file"] = inodeOf(state.MountPath("dir2/file"))

	if err := reconfigure(state.Stdin, stdoutReader, state.RootPath(), firstConfig); err != nil {
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
	if !invalidationsWork {
		t.Skipf("Kernel cache invalidations are not currently supported")
	}

	stdoutReader, stdoutWriter := io.Pipe()
	state := utils.MountSetupWithOutputs(t, stdoutWriter, os.Stderr)
	defer stdoutReader.Close()
	defer state.TearDown(t)
	defer stdoutWriter.Close()

	utils.MustMkdirAll(t, state.RootPath("dir1"), 0755)

	config := request{makeMapStep("/", mapping{Path: "/dir1", UnderlyingPath: "%ROOT%/dir1", Writable: true})}
	if err := reconfigure(state.Stdin, stdoutReader, state.RootPath(), config); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(state.MountPath("dir1/dir2"), 0755); err != nil {
		t.Errorf("Failed to create entry in writable directory: %v", err)
	}

	config = request{
		makeUnmapStep("/", "/dir1"),
		makeMapStep("/", mapping{Path: "/dir1", UnderlyingPath: "%ROOT%/dir1", Writable: false}),
	}
	if err := reconfigure(state.Stdin, stdoutReader, state.RootPath(), config); err != nil {
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

		match := false
		for {
			if !scanner.Scan() {
				if err := scanner.Err(); err != io.EOF && err != io.ErrClosedPipe {
					t.Errorf("Got error while reading from stderr: %v", err)
				}
				break
			}

			fmt.Fprintln(os.Stderr, scanner.Text())

			if utils.MatchesRegexp(pattern, scanner.Text()) {
				match = true
				found <- true
			}
		}
		if !match {
			found <- false
		}
	}

	stderrReader, stderrWriter := io.Pipe()
	defer stderrReader.Close()
	defer stderrWriter.Close()

	stdoutReader, stdoutWriter := io.Pipe()
	state := utils.MountSetupWithOutputs(t, stdoutWriter, stderrWriter)
	defer stdoutReader.Close()
	defer state.TearDown(t)
	defer stdoutWriter.Close()

	gotEOF := make(chan bool)
	go grepStderr(stderrReader, `Reached end of reconfiguration input`, gotEOF)

	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)
	config := request{makeMapStep("/", mapping{Path: "/dir", UnderlyingPath: "%ROOT%/dir", Writable: true})}
	if err := reconfigure(state.Stdin, stdoutReader, state.RootPath(), config); err != nil {
		t.Fatal(err)
	}

	if err := state.Stdin.Close(); err != nil {
		t.Fatalf("Failed to close stdin: %v", err)
	}
	state.Stdin = nil // Tell state.TearDown that we cleaned up ourselves.
	match := <-gotEOF
	if !match {
		t.Errorf("EOF not detected by sandboxfs")
	}

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
			"Input",
			"--input=" + nonExistentFile,
			fmt.Sprintf("Failed to open reconfiguration input '%s': No such file or directory", nonExistentFile),
		},
		{
			"Output",
			"--output=" + nonExistentFile,
			fmt.Sprintf("Failed to open reconfiguration output '%s': No such file or directory", nonExistentFile),
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
