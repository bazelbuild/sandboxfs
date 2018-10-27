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
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/bazelbuild/sandboxfs/integration/utils"
	"github.com/bazelbuild/sandboxfs/internal/sandbox"
)

func TestReadOnly_DirectoryStructure(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=ro:/:%ROOT%", "--mapping=ro:/mappings/dir:%ROOT%/mappings/dir", "--mapping=ro:/mappings/scaffold/dir:%ROOT%/mappings/dir")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir1"), 0755)
	utils.MustMkdirAll(t, state.RootPath("dir2"), 0500)
	utils.MustMkdirAll(t, state.RootPath("dir3/dir1"), 0700)
	utils.MustMkdirAll(t, state.RootPath("dir3/dir2"), 0755)

	// The mappings directory within the mount point will contain two entries: an explicit
	// directory that corresponds to a mapping, and an intermediate scaffold directory that only
	// exists in-memory. Create what we expect on disk so we can compare the contents later.
	utils.MustMkdirAll(t, state.RootPath("mappings/dir"), 0555)
	utils.MustMkdirAll(t, state.RootPath("mappings/scaffold"), 0555)
	if err := os.Chmod(state.RootPath("mappings"), 0555); err != nil {
		t.Fatalf("Failed to set permissions on temporary directory: %v", err)
	}
	defer os.Chmod(state.RootPath("mappings"), 0755)

	for _, dir := range []string{"", "dir1", "dir2", "dir3/dir1", "dir3/dir2", "mappings"} {
		if err := utils.DirEquals(state.RootPath(dir), state.MountPath(dir)); err != nil {
			t.Error(err)
		}
	}
}

func TestReadOnly_FileContents(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=ro:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustWriteFile(t, state.RootPath("file"), 0400, "foo")
	utils.MustMkdirAll(t, state.RootPath("dir1/dir2"), 0755)
	utils.MustWriteFile(t, state.RootPath("dir1/dir2/file"), 0600, "bar baz")

	// Do the checks many times to ensure file reads and handles do not conflict with each
	// other, and that we do not leak file descriptors within sandboxfs.
	for i := 0; i < 1000; i++ {
		if err := utils.FileEquals(state.MountPath("file"), "foo"); err != nil {
			t.Error(err)
		}
		if err := utils.FileEquals(state.MountPath("dir1/dir2/file"), "bar baz"); err != nil {
			t.Error(err)
		}
	}
}

func TestReadOnly_ReplaceUnderlyingFile(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=ro:/:%ROOT%")
	defer state.TearDown(t)

	externalFile := state.RootPath("foo")
	internalFile := state.MountPath("foo")

	utils.MustWriteFile(t, externalFile, 0600, "old contents")
	if err := utils.FileEquals(internalFile, "old contents"); err != nil {
		t.Fatalf("Test file doesn't match expected contents: %v", err)
	}

	utils.MustWriteFile(t, externalFile, 0600, "new contents")
	err := utils.FileEquals(internalFile, "new contents")
	// The behavior we get for this test on macOS and on Linux is different, and it is yet not
	// clear why that is.  In principle, Linux is right here, but let's also check the current
	// known behavior on macOS so that we can catch when it ever changes.
	// TODO(jmmv): Investigate and fix the inconsistency.
	switch runtime.GOOS {
	case "darwin":
		if err == nil {
			t.Fatalf("Test file matches expected contents, but we know it shouldn't have on this platform")
		}
	case "linux":
		if err != nil {
			t.Fatalf("Test file doesn't match expected contents: %v", err)
		}
	default:
		t.Fatalf("Don't know how this test behaves in this platform")
	}
}

func TestReadOnly_MoveUnderlyingDirectory(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=ro:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("first/a"), 0755)
	utils.MustMkdirAll(t, state.RootPath("first/b"), 0755)
	utils.MustMkdirAll(t, state.RootPath("first/c"), 0755)
	utils.MustMkdirAll(t, state.RootPath("second/1"), 0755)

	if err := utils.DirEquals(state.RootPath("first"), state.MountPath("first")); err != nil {
		t.Fatal(err)
	}
	if err := utils.DirEquals(state.RootPath("second"), state.MountPath("second")); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(state.RootPath("first"), state.RootPath("third")); err != nil {
		t.Fatalf("Failed to move underlying directory away: %v", err)
	}
	if err := os.Rename(state.RootPath("second"), state.RootPath("first")); err != nil {
		t.Fatalf("Failed to replace previous underlying directory: %v", err)
	}

	if err := utils.DirEquals(state.RootPath("first"), state.MountPath("first")); err != nil {
		t.Error(err)
	}
	if err := utils.DirEquals(state.RootPath("third"), state.MountPath("third")); err != nil {
		t.Error(err)
	}
}

func TestReadOnly_RepeatedReadDirsWhileDirIsOpen(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=ro:/:%ROOT%", "--mapping=ro:/dir:%ROOT%/dir", "--mapping=ro:/scaffold/abc:%ROOT%/dir")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("mapped-dir"), 0755)
	utils.MustWriteFile(t, state.RootPath("mapped-file"), 0644, "")
	utils.MustMkdirAll(t, state.RootPath("dir/mapped-dir-2"), 0755)
	utils.MustWriteFile(t, state.RootPath("dir/mapped-file-2"), 0644, "")

	testData := []struct {
		name string

		dir       string
		wantNames []string // Must be lexicographically sorted.
	}{
		{"Root", "/", []string{"dir", "mapped-dir", "mapped-file", "scaffold"}},
		{"MappedDir", "/dir", []string{"mapped-dir-2", "mapped-file-2"}},
		{"ScaffoldDir", "/scaffold", []string{"abc"}},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			path := state.MountPath(d.dir)

			handle, err := os.OpenFile(path, os.O_RDONLY, 0)
			if err != nil {
				t.Fatalf("Failed to open directory %s: %v", path, err)
			}
			defer handle.Close()

			// Read the contents of the directory a few times and ensure they are valid
			// every time.  Keeping the handle open used to cause subsequent reads to be
			// incomplete because the open file descriptor wouldn't be rewound. Trying
			// twice should be sufficient but it doesn't hurt to try a few more times.
			for i := 0; i < 5; i++ {
				err := utils.DirEntryNamesEqual(path, d.wantNames)
				if err != nil {
					t.Errorf("Failed iteration %d: %v", i, err)
				}
			}
		})
	}
}

func TestReadOnly_Attributes(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=ro:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)
	utils.MustWriteFile(t, state.RootPath("file"), 0644, "new content")
	utils.MustSymlink(t, "missing", state.RootPath("symlink"))

	for _, name := range []string{"dir", "file", "symlink"} {
		outerPath := state.RootPath(name)
		outerFileInfo, err := os.Lstat(outerPath)
		if err != nil {
			t.Fatalf("Failed to stat %s: %v", outerPath, err)
		}
		outerStat := outerFileInfo.Sys().(*syscall.Stat_t)

		innerPath := state.MountPath(name)
		innerFileInfo, err := os.Lstat(innerPath)
		if err != nil {
			t.Fatalf("Failed to stat %s: %v", innerPath, err)
		}
		innerStat := innerFileInfo.Sys().(*syscall.Stat_t)

		if innerFileInfo.Mode() != outerFileInfo.Mode() {
			t.Errorf("Got mode %v for %s, want %v", innerFileInfo.Mode(), innerPath, outerFileInfo.Mode())
		}

		if sandbox.Atime(innerStat) != sandbox.Atime(outerStat) {
			t.Errorf("Got atime %v for %s, want %v", sandbox.Atime(innerStat), innerPath, sandbox.Atime(outerStat))
		}
		if innerFileInfo.ModTime() != outerFileInfo.ModTime() {
			t.Errorf("Got mtime %v for %s, want %v", innerFileInfo.ModTime(), innerPath, outerFileInfo.ModTime())
		}
		if sandbox.Ctime(innerStat) != sandbox.Ctime(outerStat) {
			t.Errorf("Got ctime %v for %s, want %v", sandbox.Ctime(innerStat), innerPath, sandbox.Ctime(outerStat))
		}

		// Even though we ignore underlying link counts, we expect these internal files to
		// match the external ones because we did not create additional hard links for them.
		if innerStat.Nlink != outerStat.Nlink {
			t.Errorf("Got nlink %v for %s, want %v", innerStat.Nlink, innerPath, outerStat.Nlink)
		}

		if innerStat.Rdev != outerStat.Rdev {
			t.Errorf("Got rdev %v for %s, want %v", innerStat.Rdev, innerPath, outerStat.Rdev)
		}

		if innerStat.Blksize != outerStat.Blksize {
			t.Errorf("Got blocksize %v for %s, want %v", innerStat.Blksize, innerPath, outerStat.Blksize)
		}
	}
}

func TestReadOnly_Access(t *testing.T) {
	// mustMkdirAs creates a directory owned by the requested user and with the given mode, and
	// fails the test immediately if these operations fail.
	mustMkdirAs := func(user *utils.UnixUser, path string, mode os.FileMode) {
		cmd := exec.Command("mkdir", path)
		utils.SetCredential(cmd, user)
		if err := cmd.Run(); err != nil {
			t.Fatalf("Failed to mkdir %s as %v: %v", path, user, err)
		}

		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("Failed to chmod %v %s: %v", mode, path, err)
		}
	}

	// testAs runs test(1) as the given user to check for the access permissions requested by
	// "op" and returns the exit status of the invocation.  "op" is a test(1) flag of the form
	// "-e".
	testAs := func(user *utils.UnixUser, path string, op string) error {
		cmd := exec.Command("test", op, path)
		utils.SetCredential(cmd, user)
		return cmd.Run()
	}

	root := utils.RequireRoot(t, "Requires root privileges to test permissions as various user combinations")

	user, err := utils.LookupUserOtherThan(root.Username)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Using unprivileged user: %v", user)

	// We use MountSetupWithUser to mount the file system as root even if we are running as
	// root because this function takes care of opening up all temporary directories to all
	// readers, which we need for the tests below that run as the unprivileged user.
	//
	// Note also that we must mount with "allow=other" so that our unprivileged executions
	// can access the file system.
	state := utils.MountSetupWithUser(t, root, "--allow=other", "--mapping=ro:/:%ROOT%", "--mapping=ro:/scaffold/dir/foo:%ROOT%/foo")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("all"), 0777) // Place where "user" can create entries.

	mustMkdirAs(root, state.RootPath("all/root"), 0755)
	mustMkdirAs(root, state.RootPath("all/root/self"), 0700)
	mustMkdirAs(root, state.RootPath("all/root/self/hidden"), 0500)
	mustMkdirAs(root, state.RootPath("all/root/everyone-ro"), 0555)
	mustMkdirAs(user, state.RootPath("all/user"), 0755)
	mustMkdirAs(user, state.RootPath("all/user/self"), 0700)
	mustMkdirAs(user, state.RootPath("all/user/self/hidden"), 0500)
	mustMkdirAs(user, state.RootPath("all/user/everyone-ro"), 0555)

	testData := []struct {
		name string

		runAs    *utils.UnixUser
		testFile string
		testOp   string
		wantOk   bool
	}{
		{"RootCanLookupUser", root, "all/user/self/hidden", "-e", true},
		{"RootCanLookupRoot", root, "all/root/self/hidden", "-e", true},
		{"RootCanReadUser", root, "all/user/self", "-r", true},
		{"RootCanReadRoot", root, "all/root/self", "-r", true},
		{"RootCanWriteUser", root, "all/user/self", "-w", true},
		{"RootCanWriteRoot", root, "all/root/self", "-w", true},
		{"RootCanExecuteUser", root, "all/user/self", "-x", true},
		{"RootCanExecuteRoot", root, "all/root/self", "-x", true},

		{"RootCanReadOwnReadOnly", root, "all/root/everyone-ro", "-r", true},
		{"RootCanWriteOwnReadOnly", root, "all/root/everyone-ro", "-w", true},

		{"UserCanLookupUser", user, "all/user/self/hidden", "-e", true},
		{"UserCannotLookupRoot", user, "all/root/self/hidden", "-e", false},
		{"UserCanReadUser", user, "all/user/self", "-r", true},
		{"UserCannotReadRoot", user, "all/root/self", "-r", false},
		{"UserCanWriteUser", user, "all/user/self", "-w", true},
		{"UserCannotWriteRoot", user, "all/root/self", "-w", false},
		{"UserCanExecuteUser", user, "all/user/self", "-x", true},
		{"UserCannotExecuteRoot", user, "all/root/self", "-x", false},

		{"UserCanReadOwnReadOnly", user, "all/user/everyone-ro", "-r", true},
		{"UserCannotWriteOwnReadOnly", user, "all/user/everyone-ro", "-w", false},

		{"RootCanLookupScaffoldDir", root, "scaffold/dir", "-e", true},
		{"RootCanReadScaffoldDir", root, "scaffold/dir", "-r", true},
		// Note that scaffold directories are immutable but access tests report them as
		// writable to root.  This is an artifact of how permission checks work on read-only
		// file systems: the permission checks are based on file ownerships and modes, not
		// on whether the file system is writable.
		{"RootCanWriteScaffoldDir", root, "scaffold/dir", "-w", true},
		{"RootCanExecuteScaffoldDir", root, "scaffold/dir", "-x", true},

		{"UserCanLookupScaffoldDir", user, "scaffold/dir", "-e", true},
		{"UserCanReadScaffoldDir", user, "scaffold/dir", "-r", true},
		{"UserCannotWriteScaffoldDir", user, "scaffold/dir", "-w", false},
		{"UserCanExecuteScaffoldDir", user, "scaffold/dir", "-x", true},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			err := testAs(d.runAs, state.MountPath(d.testFile), d.testOp)
			if d.wantOk && err != nil {
				t.Errorf("Want test %s %s to succeed; got %v", d.testOp, d.testFile, err)
			} else if !d.wantOk && err == nil {
				t.Errorf("Want test %s %s to fail; got success", d.testOp, d.testFile)
			}
		})
	}
}

func TestReadOnly_HardLinkCountsAreFixed(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=ro:/:%ROOT%", "--mapping=ro:/scaffold/dir:%ROOT%/dir")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)
	utils.MustWriteFile(t, state.RootPath("no-links"), 0644, "")
	utils.MustWriteFile(t, state.RootPath("name1"), 0644, "")
	if err := os.Link(state.RootPath("name1"), state.RootPath("name2")); err != nil {
		t.Fatalf("Failed to create hard link in underlying file system: %v", err)
	}

	testData := []struct {
		name string

		file      string
		wantNlink int
	}{
		{"MappedDir", "dir", 2},
		{"FileWithOnlyOneName", "no-links", 1},
		{"FileWithManyNames", "name1", 1},
		{"ScaffoldDir", "scaffold", 2},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			fileInfo, err := os.Lstat(state.MountPath(d.file))
			if err != nil {
				t.Fatalf("Failed to stat %s in mount point: %v", d.file, err)
			}
			stat := fileInfo.Sys().(*syscall.Stat_t)
			if int(stat.Nlink) != d.wantNlink {
				t.Errorf("Want hard link count for %s to be %d; got %d", d.file, d.wantNlink, stat.Nlink)
			}
		})
	}
}

func TestReadOnly_ReadFromDirFails(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=ro:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)

	fd, err := unix.Open(state.MountPath("dir"), unix.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("Failed to open directory %s: %v", state.MountPath("dir"), err)
	}
	defer unix.Close(fd)

	buffer := make([]byte, 1024)
	_, err = unix.Read(fd, buffer)
	if err == nil || err != unix.EISDIR {
		t.Errorf("Want error to be EISDIR; got %v", err)
	}
}

func TestReadOnly_ReaddirFromFileFails(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=ro:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustWriteFile(t, state.RootPath("file"), 0644, "")

	fd, err := unix.Open(state.MountPath("file"), unix.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("Failed to open file %s: %v", state.MountPath("file"), err)
	}
	defer unix.Close(fd)

	// It is unfortunate that the error we get in this failure condition is different across
	// operating systems, but given that this case is handled within the the FUSE library (the
	// sandboxfs process never has a chance to see the invalid request), we cannot do much.
	var wantErr error
	switch runtime.GOOS {
	case "darwin":
		wantErr = unix.EINVAL
	case "linux":
		if utils.GetConfig().RustVariant {
			wantErr = unix.EINVAL
		} else {
			wantErr = unix.ENOTDIR
		}
	default:
		t.Fatalf("Don't know how this test behaves in this platform")
	}

	buffer := make([]byte, 1024)
	_, err = unix.ReadDirent(fd, buffer)
	if err == nil || err != wantErr {
		t.Errorf("Want error to be %v; got %v", wantErr, err)
	}
}

// TODO(jmmv): Must have tests to ensure that read-only mappings are, well, read only.

// TODO(jmmv): Should have tests to check what happens when the underlying files are modified
// or removed.  It's hard to say what the behavior should be here, as a FUSE file system is
// oblivious to such modifications in the general case.

// TODO(jmmv): Must have tests to verify that files are valid mapping targets, which is what we
// promise users in the documentation.

// TODO(jmmv): Need to have a test to verify all stat(2) properties of a ScaffoldDir.  The addition
// of the HardLinkCountsAreFixed test above showed that the data was bogus for the link count, so
// it's likely other details are bogus as well.
