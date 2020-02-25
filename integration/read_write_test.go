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
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bazelbuild/sandboxfs/integration/utils"
)

// The tests in this file verify the read/write mapping.  In principle, they should ensure that the
// mapping is fully-functional, including in its read-only operations.  However, as we know that
// read/write mappings are implemented in the same way as read-only mappings, we "cheat" and only
// test here for the write-specific behaviors.

// openAndDelete opens the given file with the given mode, deletes it, and returns the open file
// handle.
func openAndDelete(path string, mode int) (int, error) {
	fd, err := syscall.Open(path, mode, 0)
	if err != nil {
		return -1, fmt.Errorf("failed to open %s: %v", path, err)
	}

	if err := os.Remove(path); err != nil {
		return -1, fmt.Errorf("failed to remove %s: %v", path, err)
	}

	return fd, nil
}

// createAsDifferentUser implements a generic test that creates entries within the mount point
// as a different user than the one running sandboxfs and checks that the created entries in the
// underlying tree has the right owner:group settings.
//
// The createAsUser lambda is a function responsible for creating the desired type of entry at the
// given path with the given credentials.
func createAsDifferentUserTest(t *testing.T, createAsUser func(string, *utils.UnixUser) error) {
	root := utils.RequireRoot(t, "Requires root privileges")

	user := utils.GetConfig().UnprivilegedUser
	if user == nil {
		t.Skipf("unprivileged user not set; must contain the name of an unprivileged user with FUSE access")
	}
	t.Logf("Using primary unprivileged user: %v", user)

	state := utils.MountSetupWithUser(t, root, "--mapping=rw:/:%ROOT%", "--allow=other")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir"), 0777)
	if err := os.Chown(state.RootPath("dir"), user.UID, user.GID); err != nil {
		t.Fatalf("Cannot create unprivileged work directory: %v", err)
	}

	wantFiles := []struct {
		path string
		user *utils.UnixUser
	}{
		{path: "dir/unprivileged", user: user},
		{path: "privileged", user: root},
	}
	for _, wantFile := range wantFiles {
		if err := createAsUser(state.MountPath(wantFile.path), wantFile.user); err != nil {
			t.Fatalf("Cannot create %s as %v: %v", wantFile.path, wantFile.user, err)
		}
	}

	// Now that all files were created, make sure they have the right ownerships.
	for _, wantFile := range wantFiles {
		// Check that the underlying file has the right ownership, and also check that the
		// node we recorded in sandboxfs' memory agrees.
		for _, path := range []string{state.MountPath(wantFile.path), state.RootPath(wantFile.path)} {
			fileInfo, err := os.Lstat(path)
			if err != nil {
				t.Fatalf("Cannot stat %s: %v", path, err)
			}
			stat := fileInfo.Sys().(*syscall.Stat_t)
			if int(stat.Uid) != wantFile.user.UID || int(stat.Gid) != wantFile.user.GID {
				t.Errorf("%s has wrong ownership; got %v:%v, want %v:%v", path, stat.Uid, stat.Gid, wantFile.user.UID, wantFile.user.GID)
			}
		}
	}
}

func TestReadWrite_MkdirAsDifferentUser(t *testing.T) {
	createAsDifferentUserTest(t, utils.MkdirAsUser)
}

func TestReadWrite_CreateFile(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustWriteFile(t, state.RootPath("file"), 0644, "original content")
	utils.MustMkdirAll(t, state.RootPath("subdir"), 0755)
	utils.MustWriteFile(t, state.MountPath("subdir/file"), 0644, "new content")

	if err := utils.FileEquals(state.MountPath("file"), "original content"); err != nil {
		t.Error(err)
	}
	if err := utils.FileEquals(state.MountPath("subdir/file"), "new content"); err != nil {
		t.Error(err)
	}
}

func TestReadWrite_CreateFileAsDifferentUser(t *testing.T) {
	createAsDifferentUserTest(t, utils.CreateFileAsUser)
}

func TestReadWrite_DirectoryNlinkCountsStayFixed(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	checkNlink := func(path string, wantNlink int) {
		t.Helper()
		var stat syscall.Stat_t
		if err := syscall.Lstat(path, &stat); err != nil {
			t.Fatalf("Lstat failed on deleted entry: %v", err)
		}
		if int(stat.Nlink) != wantNlink {
			t.Errorf("Got nlink %d, want %d", stat.Nlink, wantNlink)
		}
	}

	mustMkdir := func(path string) {
		t.Helper()
		if err := os.Mkdir(path, 755); err != nil {
			t.Fatalf("Failed to mkdir %s: %v", path, err)
		}
	}

	mustRemove := func(path string) {
		t.Helper()
		if err := os.Remove(path); err != nil {
			t.Fatalf("Failed to remove %s: %v", path, err)
		}
	}

	utils.MustMkdirAll(t, state.RootPath("subdir"), 0755)
	utils.MustMkdirAll(t, state.RootPath("subdir/dir1"), 0755)
	utils.MustMkdirAll(t, state.RootPath("subdir/dir2"), 0755)
	utils.MustWriteFile(t, state.RootPath("subdir/file"), 0644, "original content")

	checkNlink(state.MountPath("subdir"), 2)

	mustRemove(state.MountPath("subdir/dir1"))
	checkNlink(state.MountPath("subdir"), 2)

	mustRemove(state.MountPath("subdir/file"))
	checkNlink(state.MountPath("subdir"), 2)

	mustMkdir(state.MountPath("subdir/dir3"))
	checkNlink(state.MountPath("subdir"), 2)

	mustRemove(state.MountPath("subdir/dir2"))
	checkNlink(state.MountPath("subdir"), 2)

	mustRemove(state.MountPath("subdir/dir3"))
	checkNlink(state.MountPath("subdir"), 2)
}

func TestReadWrite_Remove(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%", "--mapping=rw:/mapped-dir:%ROOT%/mapped-dir", "--mapping=rw:/scaffold/dir:%ROOT%/scaffold-dir")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)
	utils.MustWriteFile(t, state.RootPath("file"), 0644, "")
	utils.MustMkdirAll(t, state.RootPath("mapped-dir"), 0755) // Clobbered by mapping.

	t.Run("MappedDirCannotBeRemoved", func(t *testing.T) {
		if err := os.Remove(state.MountPath("mapped-dir")); !os.IsPermission(err) {
			t.Errorf("Want removal of mapped directory to return permission error; got %v", err)
		}

		if _, err := os.Lstat(state.MountPath("mapped-dir")); err != nil {
			t.Errorf("Want mapped directory to remain within the mount point; got %v", err)
		}

		if _, err := os.Lstat(state.RootPath("mapped-dir")); err != nil {
			t.Errorf("Want entry clobbered by mapping to remain on disk (no Lstat error); got %v", err)
		}
	})

	t.Run("ScaffoldDirCannotBeRemoved", func(t *testing.T) {
		if err := os.Remove(state.MountPath("scaffold")); !os.IsPermission(err) {
			t.Errorf("Want removal of scaffold directory to return permission error; got %v", err)
		}

		if _, err := os.Lstat(state.MountPath("scaffold")); err != nil {
			t.Errorf("Want scaffold directory to remain within the mount point; got %v", err)
		}
	})

	t.Run("FileDoesNotExist", func(t *testing.T) {
		if err := os.Remove(state.MountPath("non-existent")); !os.IsNotExist(err) {
			t.Errorf("Want removal of non-existent file to return non-existence error; got %v", err)
		}
	})

	t.Run("EntryExists", func(t *testing.T) {
		for _, name := range []string{"dir", "file"} {
			if err := os.Remove(state.MountPath(name)); err != nil {
				t.Errorf("Want removal of existent file to succeed; got %v", err)
			}

			if _, err := os.Lstat(state.MountPath(name)); !os.IsNotExist(err) {
				t.Errorf("Want stat of removed file within mount point to report non-existence error; got %v", err)
			}

			if _, err := os.Lstat(state.RootPath(name)); !os.IsNotExist(err) {
				t.Errorf("Want stat of removed file in the underlying directory to report non-existence error; got %v", err)
			}
		}
	})
}

func TestReadWRite_RemoveZeroesNlink(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)
	utils.MustWriteFile(t, state.RootPath("file"), 0644, "")
	for _, name := range []string{"Dir", "File"} {
		t.Run(name, func(t *testing.T) {
			path := state.MountPath(strings.ToLower(name))
			fd, err := openAndDelete(path, syscall.O_RDONLY)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Close(fd)

			var stat syscall.Stat_t
			if err := syscall.Fstat(fd, &stat); err != nil {
				t.Fatalf("Fstat failed on deleted entry: %v", err)
			}
			if int(stat.Nlink) != 0 {
				t.Errorf("Bad link count: got %d, want 0", stat.Nlink)
			}
		})
	}
}

func TestReadWrite_RewriteFile(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustWriteFile(t, state.RootPath("file"), 0644, "original content")
	if err := utils.FileEquals(state.MountPath("file"), "original content"); err != nil {
		t.Error(err)
	}

	utils.MustWriteFile(t, state.MountPath("file"), 0644, "rewritten content")
	if err := utils.FileEquals(state.MountPath("file"), "rewritten content"); err != nil {
		t.Error(err)
	}
}

func TestReadWrite_RewriteFileWithShorterContent(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustWriteFile(t, state.MountPath("file"), 0644, "very long contents")
	utils.MustWriteFile(t, state.MountPath("file"), 0644, "short")
	if err := utils.FileEquals(state.MountPath("file"), "short"); err != nil {
		t.Error(err)
	}
}

func TestReadWrite_WriteOnDeletedAndDuppedFd(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	fd, err := openAndDelete(state.MountPath("some-file"), syscall.O_RDWR|syscall.O_CREAT)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)

	mustWrite := func(fd int, s string) {
		t.Helper()
		if n, err := syscall.Write(fd, []byte(s)); err != nil || n != len(s) {
			t.Fatalf("Failed to write: got n=%d, err=%v; want n=%d, err=nil", n, err, len(s))
		}
	}

	mustWrite(fd, "123")

	fd2, err := syscall.Dup(fd)
	if err != nil {
		t.Fatalf("Dup failed: %v", err)
	}
	defer syscall.Close(fd2)
	syscall.Close(fd)

	mustWrite(fd2, "45")

	if n, err := syscall.Seek(fd2, 0, io.SeekStart); err != nil || n != 0 {
		t.Fatalf("Failed to rewind file: n=%d, err=%v", n, err)
	}

	wantBuf := []byte("12345")
	buf := make([]byte, 8)
	if n, err := syscall.Read(fd2, buf); err != nil || n != len(wantBuf) {
		t.Fatalf("Failed to write: got n=%d, err=%v; want n=%d, err=nil", n, err, len(wantBuf))
	} else {
		buf = buf[0:n]
	}

	if !reflect.DeepEqual(buf, wantBuf) {
		t.Errorf("Data read from file doesn't match written: got %v, want %v", buf, wantBuf)
	}

	var stat syscall.Stat_t
	if err := syscall.Fstat(fd2, &stat); err != nil {
		t.Fatalf("Fstat failed on deleted entry: %v", err)
	}
	if stat.Size != int64(len(wantBuf)) {
		t.Errorf("Bad file length: got %d, want %d", stat.Size, len(wantBuf))
	}
}

func TestReadWrite_InodesArePreservedDuringReaddir(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	// inodeOf obtains the inode number of a file.
	inodeOf := func(path string) uint64 {
		fileInfo, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("Failed to get inode number of %s: %v", path, err)
		}
		return fileInfo.Sys().(*syscall.Stat_t).Ino
	}

	utils.MustMkdirAll(t, state.MountPath("dir"), 0755)
	utils.MustWriteFile(t, state.MountPath("file"), 0644, "")

	for _, name := range []string{"dir", "file"} {
		previous := inodeOf(state.MountPath(name))
		if found, err := existsViaReaddir(state.MountPath(), name); err != nil || !found {
			t.Fatalf("Cannot find entry %s: %v", name, err)
		}

		// Rename the entry to force the kernel to "rediscover" it during the next readdir.
		// Otherwise, we might not trigger bugs with inode preservation, and the behavior of
		// this depends on the platform.
		newName := name + "-new"
		if err := os.Rename(state.MountPath(name), state.MountPath(newName)); err != nil {
			t.Fatalf("Cannot rename %s: %v", name, err)
		}

		if found, err := existsViaReaddir(state.MountPath(), newName); err != nil || !found {
			t.Fatalf("Cannot find entry %s: %v", newName, err)
		}
		now := inodeOf(state.MountPath(newName))
		if previous != now {
			t.Errorf("Inode number was not stable for %s; got %v, want %v", name, now, previous)
		}
	}
}

func TestReadWrite_InodeReassignedAfterRecreation(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	testData := []struct {
		name string

		path   string
		create func(string, os.FileMode)
	}{
		{
			"Dir",
			"dir",
			func(path string, mode os.FileMode) { utils.MustMkdirAll(t, path, mode) },
		},
		{
			"File",
			"file",
			func(path string, mode os.FileMode) { utils.MustWriteFile(t, path, mode, "") },
		},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			path := state.MountPath(d.path)
			d.create(path, 0755)
			fileInfo, err := os.Lstat(path)
			if err != nil {
				t.Fatalf("Failed to get inode number of entry after first creation: %v", err)
			}
			originalInode := fileInfo.Sys().(*syscall.Stat_t).Ino
			originalMode := fileInfo.Mode()

			if err := os.Remove(path); err != nil {
				t.Fatalf("Failed to remove entry: %v", err)
			}

			d.create(path, 0700)
			fileInfo, err = os.Lstat(path)
			if err != nil {
				t.Fatalf("Failed to get inode number of entry after recreation: %v", err)
			}
			recreatedInode := fileInfo.Sys().(*syscall.Stat_t).Ino
			recreatedMode := fileInfo.Mode()

			if originalInode == recreatedInode {
				t.Errorf("Still got inode number %v; want it to change after entry recreation", recreatedInode)
			}
			// Checking the mode's equality feels a bit out of scope, but it's not: an "inode" contains
			// both the inode number and all other file metadata.  Checking for mode equality is just a
			// simple test to ensure that such metadata is not shared.
			if originalMode == recreatedMode {
				t.Errorf("Still got file mode %v; want it to change after entry recreation with different permissions", recreatedMode)
			}
		})
	}
}

func TestReadWrite_FstatOnDeletedNode(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.MountPath("dir"), 0755)
	utils.MustWriteFile(t, state.MountPath("file"), 0644, "")

	testData := []struct {
		name         string
		relativePath string
	}{
		{"MappedDir", "dir"},
		{"MappedFile", "file"},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			path := state.MountPath(d.relativePath)

			var wantStat syscall.Stat_t
			if err := syscall.Stat(path, &wantStat); err != nil {
				t.Fatalf("Fstat failed on golden entry: %v", err)
			}

			fd, err := openAndDelete(path, syscall.O_RDONLY)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Close(fd)

			var stat syscall.Stat_t
			if err := syscall.Fstat(fd, &stat); err != nil {
				t.Fatalf("Fstat failed on deleted entry: %v", err)
			}
			// TODO(jmmv): It's not true that the stats should be fully equal.  In
			// particular, Nlink should have decreased to zero after deletion... but we
			// currently do not explicitly do this and the behavior seems to be
			// system-dependent.  So, for now, just ignore that field.
			stat.Nlink = 0
			wantStat.Nlink = 0
			if stat != wantStat {
				t.Errorf("Got stat %v; want %v", stat, wantStat)
			}
		})
	}
}

func TestReadWrite_Truncate(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustWriteFile(t, state.MountPath("file"), 0644, "very long contents")

	wantContent := "very"
	if err := os.Truncate(state.MountPath("file"), int64(len(wantContent))); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	if err := utils.FileEquals(state.MountPath("file"), wantContent); err != nil {
		t.Error(err)
	}
}

func TestReadWrite_FtruncateOnDeletedFile(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	originalContent := "very long contents"
	utils.MustWriteFile(t, state.MountPath("file"), 0644, originalContent)

	fd, err := openAndDelete(state.MountPath("file"), syscall.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)

	wantContent := "very"
	if err := syscall.Ftruncate(fd, int64(len(wantContent))); err != nil {
		t.Fatalf("Ftruncate on deleted file failed: %v", err)
	}

	buf := make([]byte, len(originalContent))
	n, err := syscall.Read(fd, buf)
	if err != nil {
		t.Fatalf("Failed to read from truncated file: %v", err)
	}
	if n != len(wantContent) {
		t.Errorf("Got %d bytes from truncated file; want %d", n, len(wantContent))
	}
	buf = buf[:n]
	if string(buf) != wantContent {
		t.Errorf("Got content %s; want %s", string(buf), wantContent)
	}
}

// sameInode compares two os.FileInfo objects and returns true if they refer to the same inode.
func sameInode(stat1 os.FileInfo, stat2 os.FileInfo) bool {
	ino1 := stat1.Sys().(*syscall.Stat_t).Ino
	ino2 := stat2.Sys().(*syscall.Stat_t).Ino

	return ino1 == ino2
}

// equivalentStats compares two os.FileInfo objects and returns nil if they represent the same
// file; otherwise returns a descriptive error including the differences between the two.
// This equivalency is to be used during file move tests, to check if a file was actually moved
// instead of recreated.
func equivalentStats(stat1 os.FileInfo, stat2 os.FileInfo) error {
	ino1 := stat1.Sys().(*syscall.Stat_t).Ino
	ino2 := stat2.Sys().(*syscall.Stat_t).Ino

	if stat1.Mode() != stat2.Mode() || stat1.ModTime() != stat2.ModTime() || ino1 != ino2 {
		return fmt.Errorf("got mode=%v, mtime=%v, inode=%v; want mode=%v, mtime=%v, inode=%v", stat1.Mode(), stat1.ModTime(), ino1, stat2.Mode(), stat2.ModTime(), ino2)
	}
	return nil
}

// doRenameTest is a helper function for the tests that verify the file system-level rename
// operation.  This takes the path of a file to be moved (the "old outer path"), the path of the
// rename target (the "new outer path"), and the corresponding paths within the mount point.
//
// Tests calling this function should only start a sandboxfs instance with the desired configuration
// and then immediately call this function.
func doRenameTest(t *testing.T, oldOuterPath, newOuterPath, oldInnerPath, newInnerPath string) {
	utils.MustMkdirAll(t, filepath.Dir(oldOuterPath), 0755)
	utils.MustMkdirAll(t, filepath.Dir(newOuterPath), 0755)
	utils.MustMkdirAll(t, filepath.Dir(oldInnerPath), 0755)
	utils.MustMkdirAll(t, filepath.Dir(newInnerPath), 0755)
	utils.MustWriteFile(t, oldOuterPath, 0644, "some content")

	lstatOrFatal := func(path string) os.FileInfo {
		stat, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("Failed to lstat %s: %v", path, err)
		}
		return stat
	}
	oldOuterStat := lstatOrFatal(oldOuterPath)
	oldInnerStat := lstatOrFatal(oldInnerPath)
	if err := os.Rename(oldInnerPath, newInnerPath); err != nil {
		t.Fatalf("Failed to rename %s to %s: %v", oldInnerPath, newInnerPath, err)
	}
	newOuterStat := lstatOrFatal(newOuterPath)
	newInnerStat := lstatOrFatal(newInnerPath)

	if _, err := os.Lstat(oldOuterPath); os.IsExist(err) {
		t.Fatalf("Old file name in root still present but should have disappeared: %s", oldOuterPath)
	}
	if _, err := os.Lstat(oldInnerPath); os.IsExist(err) {
		t.Fatalf("Old file name in mount point still present but should have disappeared: %s", oldInnerPath)
	}
	if err := utils.FileEquals(newOuterPath, "some content"); err != nil {
		t.Fatalf("New file name in root missing or with bad contents: %s: %v", newOuterPath, err)
	}
	if err := utils.FileEquals(newInnerPath, "some content"); err != nil {
		t.Fatalf("New file name in mount point missing or with bad contents: %s: %v", newInnerPath, err)
	}

	if !sameInode(oldOuterStat, newOuterStat) {
		t.Errorf("Inode was not preserved for %s to %s move", oldOuterPath, newOuterPath)
	}
	if !sameInode(oldInnerStat, newInnerStat) {
		t.Errorf("Inode was not preserved for %s to %s move", oldInnerPath, newInnerPath)
	}

	if err := equivalentStats(oldOuterStat, newOuterStat); err != nil {
		t.Errorf("Stats for %s and %s differ: %v", oldOuterPath, newOuterPath, err)
	}
	if err := equivalentStats(oldInnerStat, newInnerStat); err != nil {
		t.Errorf("Stats for %s and %s differ: %v", oldInnerPath, newInnerPath, err)
	}
}

func TestReadWrite_NestedMappingsInheritDirectoryProperties(t *testing.T) {
	rootSetup := func(root string) error {
		if err := os.MkdirAll(filepath.Join(root, "already/exist"), 0755); err != nil {
			return err
		}
		return os.MkdirAll(filepath.Join(root, "dir"), 0755)
	}
	state := utils.MountSetupWithRootSetup(t, rootSetup,
		"--mapping=rw:/:%ROOT%",
		"--mapping=ro:/already/exist/dir:%ROOT%/dir")
	defer state.TearDown(t)

	for _, path := range []string{"already/foo", "already/exist/foo"} {
		if err := ioutil.WriteFile(state.MountPath(path), []byte(""), 0644); err != nil {
			t.Errorf("Cannot create %s; possible mapping interference: %v", path, err)
		}
		if _, err := os.Lstat(state.RootPath(path)); err != nil {
			t.Errorf("Cannot find %s in underlying root location: %v", path, err)
		}
	}

	if err := ioutil.WriteFile(state.MountPath("already/exist/dir/foo"), []byte(""), 0644); err == nil {
		t.Errorf("Successfully created file in read-only mapping")
	}
}

func TestReadWrite_NestedMappingsClobberFiles(t *testing.T) {
	rootSetup := func(root string) error {
		if err := os.MkdirAll(filepath.Join(root, "dir"), 0755); err != nil {
			return err
		}
		if err := ioutil.WriteFile(filepath.Join(root, "file"), []byte(""), 0644); err != nil {
			return err
		}
		return os.Symlink("/non-existent", filepath.Join(root, "symlink"))
	}
	state := utils.MountSetupWithRootSetup(t, rootSetup,
		"--mapping=rw:/:%ROOT%",
		"--mapping=ro:/file/nested-dir:%ROOT%/dir",
		"--mapping=ro:/symlink/nested-dir:%ROOT%/dir")
	defer state.TearDown(t)

	for _, component := range []string{"file", "symlink"} {
		fileInfo, err := os.Lstat(state.MountPath(component, "nested-dir"))
		if err != nil {
			t.Errorf("Cannot navigate into mapping %s/nested-dir; underlying entry interfered: %v", component, err)
		}
		if fileInfo.Mode()&os.ModeType != os.ModeDir {
			t.Errorf("Got mode %v for mapping; want directory", fileInfo.Mode())
		}

		if err := os.Mkdir(state.MountPath(component, "other"), 0755); err == nil {
			t.Errorf("Intermediate mapping directory %s was not read-only", component)
		}
	}
}

func TestReadWrite_RenameFile(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	oldOuterPath := state.RootPath("old-name")
	newOuterPath := state.RootPath("new-name")
	oldInnerPath := state.MountPath("old-name")
	newInnerPath := state.MountPath("new-name")
	doRenameTest(t, oldOuterPath, newOuterPath, oldInnerPath, newInnerPath)
}

func TestReadWrite_MoveFile(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	oldOuterPath := state.RootPath("dir1/dir2/old-name")
	newOuterPath := state.RootPath("dir2/dir3/dir4/new-name")
	oldInnerPath := state.MountPath("dir1/dir2/old-name")
	newInnerPath := state.MountPath("dir2/dir3/dir4/new-name")
	doRenameTest(t, oldOuterPath, newOuterPath, oldInnerPath, newInnerPath)
}

func TestReadWrite_MoveRace(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir1"), 0755)
	utils.MustWriteFile(t, state.RootPath("dir1/file1"), 0644, "")
	utils.MustMkdirAll(t, state.RootPath("dir2"), 0755)
	utils.MustWriteFile(t, state.RootPath("dir2/file2"), 0644, "")

	doMove := func(errChan chan error, srcdir string, newdir string, file string) {
		for i := 0; i < 1000; i++ {
			if err := os.Rename(state.MountPath(srcdir, file), state.MountPath(newdir, file)); err != nil {
				errChan <- err
				return
			}
			if err := os.Rename(state.MountPath(newdir, file), state.MountPath(srcdir, file)); err != nil {
				errChan <- err
				return
			}
		}
		errChan <- nil
	}

	errChan := make(chan error)
	go doMove(errChan, "dir1", "dir2", "file1")
	go doMove(errChan, "dir2", "dir1", "file2")

	// If there is a deadlock in the implementation of rename, we expect at least one of the
	// goroutines to get stuck and never return.
	for i := 0; i < 2; i++ {
		err := <-errChan
		if err != nil {
			t.Errorf("Renames failed: %v", err)
		}
	}
}

func TestReadWrite_MoveAsDifferentUser(t *testing.T) {
	createAsDifferentUserTest(t, func(path string, user *utils.UnixUser) error {
		if err := utils.CreateFileAsUser(path+".old", user); err != nil {
			return err
		}
		return utils.MoveAsUser(path+".old", path, user)
	})
}

func TestReadWrite_MoveDirectoryUpdatesContents(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	// Create the files directly in the mount path, as opposed to the root path (like other
	// tests do), to ensure sandboxfs creates in-memory nodes for them.
	utils.MustMkdirAll(t, state.MountPath("dir1"), 0755)
	utils.MustWriteFile(t, state.MountPath("dir1/file"), 0644, "First file")
	utils.MustMkdirAll(t, state.MountPath("dir1/subdir"), 0755)
	utils.MustWriteFile(t, state.MountPath("dir1/subdir/file"), 0644, "Second file")

	if err := os.Rename(state.MountPath("dir1"), state.MountPath("dir2")); err != nil {
		t.Fatalf("Rename failed: %v", err)
	}

	if err := utils.FileEquals(state.MountPath("dir2/file"), "First file"); err != nil {
		t.Fatalf("dir2/file is invalid: %v", err)
	}
	if err := utils.FileEquals(state.MountPath("dir2/subdir/file"), "Second file"); err != nil {
		t.Fatalf("dir2/subdir/file is invalid: %v", err)
	}
}

func TestReadWrite_DeleteAfterMovedDirectory(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	// Create the files directly in the mount path, as opposed to the root path (like other
	// tests do), to ensure sandboxfs creates in-memory nodes for them.
	utils.MustMkdirAll(t, state.MountPath("dir1"), 0755)
	utils.MustWriteFile(t, state.MountPath("dir1/file"), 0644, "First file")
	utils.MustMkdirAll(t, state.MountPath("dir1/subdir"), 0755)
	utils.MustWriteFile(t, state.MountPath("dir1/subdir/file"), 0644, "Second file")

	if err := os.Rename(state.MountPath("dir1"), state.MountPath("dir2")); err != nil {
		t.Fatalf("Rename failed: %v", err)
	}

	if err := os.RemoveAll(state.MountPath("dir2")); err != nil {
		t.Errorf("Remove failed: %v", err)
	}
}

func TestReadWrite_Mknod(t *testing.T) {
	utils.RequireRoot(t, "Requires root privileges to create arbitrary nodes")

	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	// checkNode ensures that a given file is of the specified type and, if the type indicates
	// that the file is a device, that the device number matches.  This check is done on both
	// the underlying file system and within the mount point.
	checkNode := func(relPath string, wantMode os.FileMode, wantDev uint64) error {
		for _, path := range []string{state.RootPath(relPath), state.MountPath(relPath)} {
			fileInfo, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("failed to stat %s: %v", path, err)
			}
			stat := fileInfo.Sys().(*syscall.Stat_t)

			if fileInfo.Mode() != wantMode {
				return fmt.Errorf("got mode %v for %s, want %v", fileInfo.Mode(), path, wantMode)
			}
			if (wantMode&os.ModeType)&os.ModeDevice != 0 {
				if uint64(stat.Rdev) != wantDev { // stat.Rdev size and sign are platform-specific.
					return fmt.Errorf("got dev %v for %s, want %v", stat.Rdev, path, wantDev)
				}
			}
		}
		return nil
	}

	// findOS checks if the current OS appears in a list of acceptable OSes.
	findOS := func(oses []string) bool {
		for _, os := range oses {
			if os == runtime.GOOS {
				return true
			}
		}
		return false
	}

	allOSes := []string{"darwin", "linux"}
	if !findOS(allOSes) {
		t.Fatalf("Don't know how this test behaves in this platform")
	}

	testData := []struct {
		name string

		filename  string
		perm      uint32
		mknodType uint32
		dev       int
		statType  os.FileMode

		// The behavior of mknod(2) is operating-system specific.  On Linux, we can create
		// regular files with this call, and attempting to create a directory results in the
		// wrong node being created.  On macOS, attempting to create either of these fails.
		//
		// Instead of ignoring these cases as invalid, test specifically for the behavior we
		// know should happen by "whitelisting" the systems on which each test is valid.
		// This way, we verify that sandboxfs is properly delegating these calls to the
		// underlying system.
		wantOS []string
	}{
		{"RegularFile", "file", 0644, syscall.S_IFREG, 0, 0, []string{"linux"}},
		{"Directory", "dir", 0755, syscall.S_IFDIR, 0, os.ModeDir, []string{}},
		{"BlockDevice", "blkdev", 0400, syscall.S_IFBLK, 1234, os.ModeDevice, allOSes},
		{"CharDevice", "chrdev", 0400, syscall.S_IFCHR, 5678, os.ModeDevice | os.ModeCharDevice, allOSes},
		{"NamedPipe", "fifo", 0640, syscall.S_IFIFO, 0, os.ModeNamedPipe, allOSes},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			path := state.MountPath(d.filename)

			shouldHaveFailed := false

			err := syscall.Mknod(path, d.perm|d.mknodType, d.dev)
			if findOS(d.wantOS) {
				if err != nil {
					t.Fatalf("Failed to mknod %s: %v", path, err)
				}
			} else {
				if err == nil {
					shouldHaveFailed = true
				}
			}

			err = checkNode(d.filename, (os.FileMode(d.perm)&os.ModePerm)|d.statType, uint64(d.dev))
			if findOS(d.wantOS) {
				if err != nil {
					t.Error(err)
				}
			} else {
				if err == nil {
					shouldHaveFailed = true
				}
			}

			if shouldHaveFailed {
				t.Fatalf("Test was expected to fail on this platform due to behavioral differences in mknod(2) but succeeded")
			}
		})
	}
}
func TestReadWrite_MknodAsDifferentUser(t *testing.T) {
	createAsDifferentUserTest(t, utils.MkfifoAsUser)
}

func TestReadWrite_Chmod(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	// checkPerm ensures that the given file has the given permissions on the underlying file
	// system and within the mount point.
	checkPerm := func(relPath string, wantPerm os.FileMode) error {
		for _, path := range []string{state.RootPath(relPath), state.MountPath(relPath)} {
			fileInfo, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("failed to stat %s: %v", path, err)
			}
			perm := fileInfo.Mode() & os.ModePerm
			if perm != wantPerm {
				return fmt.Errorf("got permissions %v for %s, want %v", perm, path, wantPerm)
			}
		}
		return nil
	}

	t.Run("Dir", func(t *testing.T) {
		utils.MustMkdirAll(t, state.RootPath("dir"), 0755)

		path := state.MountPath("dir")
		if err := os.Chmod(path, 0500); err != nil {
			t.Fatalf("Failed to chmod %s: %v", path, err)
		}
		if err := checkPerm("dir", 0500); err != nil {
			t.Error(err)
		}
	})

	t.Run("File", func(t *testing.T) {
		utils.MustWriteFile(t, state.RootPath("file"), 0644, "new content")

		path := state.MountPath("file")
		if err := os.Chmod(path, 0440); err != nil {
			t.Fatalf("Failed to chmod %s: %v", path, err)
		}
		if err := checkPerm("file", 0440); err != nil {
			t.Error(err)
		}
	})

	t.Run("Symlink", func(t *testing.T) {
		utils.MustWriteFile(t, state.RootPath("target"), 0644, "")
		utils.MustSymlink(t, "target", state.RootPath("symlink"))

		path := state.MountPath("symlink")
		linkFileInfo, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("Failed to stat %s: %v", path, err)
		}

		if err := os.Chmod(path, 0200); err != nil {
			t.Fatalf("Failed to chmod %s: %v", path, err)
		}

		if err := checkPerm("symlink", linkFileInfo.Mode()&os.ModePerm); err != nil {
			t.Error(err)
		}
		if err := checkPerm("target", 0200); err != nil {
			t.Errorf("Mode of symlink target was modified but shouldn't have been: %v", err)
		}
	})
}

func TestReadWrite_FchmodOnDeletedNode(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.MountPath("dir"), 0755)
	utils.MustWriteFile(t, state.MountPath("file"), 0644, "")

	testData := []struct {
		name         string
		relativePath string
	}{
		{"MappedDir", "dir"},
		{"MappedFile", "file"},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			path := state.MountPath(d.relativePath)

			fd, err := openAndDelete(path, syscall.O_RDONLY)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Close(fd)

			if err := syscall.Fchmod(fd, 0444); err != nil {
				t.Fatalf("Fchmod failed on deleted entry: %v", err)
			}

			var stat syscall.Stat_t
			if err := syscall.Fstat(fd, &stat); err != nil {
				t.Fatalf("Fstat failed on deleted entry: %v", err)
			}
			if stat.Mode&^syscall.S_IFMT != 0444 {
				t.Errorf("Want file mode %o, got %o", 0444, stat.Mode&^syscall.S_IFMT)
			}
		})
	}
}

func TestReadWrite_Chown(t *testing.T) {
	utils.RequireRoot(t, "Requires root privileges to change test file ownership")

	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	// checkOwners ensures that the given file is owned by the given user and group on the
	// underlying file system and within the mount point.
	checkOwners := func(relPath string, wantUID uint32, wantGID uint32) error {
		for _, path := range []string{state.RootPath(relPath), state.MountPath(relPath)} {
			fileInfo, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("failed to stat %s: %v", path, err)
			}
			stat := fileInfo.Sys().(*syscall.Stat_t)

			if stat.Uid != wantUID {
				return fmt.Errorf("got uid %v for %s, want %v", stat.Uid, path, wantUID)
			}
			if stat.Gid != wantGID {
				return fmt.Errorf("got gid %v for %s, want %v", stat.Gid, path, wantGID)
			}
		}
		return nil
	}

	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)
	utils.MustWriteFile(t, state.RootPath("file"), 0644, "new content")
	utils.MustWriteFile(t, state.RootPath("target"), 0644, "")
	utils.MustSymlink(t, "target", state.RootPath("symlink"))

	targetFileInfo, err := os.Lstat(state.RootPath("target"))
	if err != nil {
		t.Fatalf("Failed to stat %s: %v", state.RootPath("target"), err)
	}
	targetStat := targetFileInfo.Sys().(*syscall.Stat_t)

	testData := []struct {
		name string

		filename string
		wantUID  int
		wantGID  int
	}{
		{"Dir", "dir", 1, 2},
		{"File", "file", 3, 4},
		{"Symlink", "symlink", 7, 8},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			path := state.MountPath(d.filename)
			if err := os.Lchown(path, d.wantUID, d.wantGID); err != nil {
				t.Fatalf("Failed to chown %s: %v", path, err)
			}
			if err := checkOwners(d.filename, uint32(d.wantUID), uint32(d.wantGID)); err != nil {
				t.Error(err)
			}
		})
	}

	if err := checkOwners("target", targetStat.Uid, targetStat.Gid); err != nil {
		t.Errorf("Ownership of symlink target was modified but shouldn't have been: %v", err)
	}
}

func TestReadWrite_FchownOnDeletedNode(t *testing.T) {
	utils.RequireRoot(t, "Requires root privileges to change test file ownership")

	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.MountPath("dir"), 0755)
	utils.MustWriteFile(t, state.MountPath("file"), 0644, "")

	testData := []struct {
		name         string
		relativePath string
	}{
		{"MappedDir", "dir"},
		{"MappedFile", "file"},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			path := state.MountPath(d.relativePath)

			fd, err := openAndDelete(path, syscall.O_RDONLY)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Close(fd)

			if err := syscall.Fchown(fd, 10, 20); err != nil {
				t.Fatalf("Fchown failed on deleted entry: %v", err)
			}

			var stat syscall.Stat_t
			if err := syscall.Fstat(fd, &stat); err != nil {
				t.Fatalf("Fstat failed on deleted entry: %v", err)
			}
			if stat.Uid != 10 || stat.Gid != 20 {
				t.Errorf("Want uid 10, gid 20; got uid %d, gid %d", stat.Uid, stat.Gid)
			}
		})
	}
}

func TestReadWrite_Chtimes(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	// checkTimes ensures that the given file has the desired timing information on the
	// underlying file system and within the mount point.
	//
	// wantAtime may be zero if the atime check should be skipped.  wantMtime is always checked
	// for equality.  wantMinCtime indicates the minimum ctime that the file should have, as
	// that's the most we can check for (because ctime cannot be explicitly set).
	checkTimes := func(relPath string, wantAtime time.Time, wantMtime time.Time, wantMinCtime time.Time) error {
		for _, path := range []string{state.RootPath(relPath), state.MountPath(relPath)} {
			fileInfo, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("failed to stat %s: %v", path, err)
			}
			stat := fileInfo.Sys().(*syscall.Stat_t)

			if !fileInfo.ModTime().Equal(wantMtime) {
				return fmt.Errorf("got mtime %v for %s, want %v", fileInfo.ModTime(), path, wantMtime)
			}
			if !wantAtime.Equal(time.Unix(0, 0)) && !utils.Atime(stat).Equal(wantAtime) {
				return fmt.Errorf("got atime %v for %s, want %v", utils.Atime(stat), path, wantAtime)
			}
			if utils.Ctime(stat).Before(wantMinCtime) {
				return fmt.Errorf("got ctime %v for %s, want <= %v", utils.Ctime(stat), path, wantMinCtime)
			}
		}
		return nil
	}

	// chtimes is a wrapper over os.Chtimes that updates the given file with the desired atime
	// and mtime, but also computes a lower bound for the ctime of the touched file.  This lower
	// bound is returned and can later be fed to checkTimes.
	chtimes := func(path string, atime time.Time, mtime time.Time) (time.Time, error) {
		// We have no control on ctime updates so let some time pass before we modify our
		// test file.  This way, we can ensure that the ctime was set to, at least, the
		// current updated time.  All file systems should have a minimum of second-level
		// granularity (I'm looking at you HFS+), so sleeping for a whole second should be
		// sufficient to get this right.  (Sleeps can pause for longer than specified, but
		// that's perfectly fine.)
		minCtime := time.Now()
		time.Sleep(1 * time.Second)

		atimeTimespec, err := unix.TimeToTimespec(atime)
		if err != nil {
			t.Fatalf("Failed to convert %v to a timespec: %v", atime, err)
		}
		mtimeTimespec, err := unix.TimeToTimespec(mtime)
		if err != nil {
			t.Fatalf("Failed to convert %v to a timespec: %v", mtime, err)
		}

		if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{atimeTimespec, mtimeTimespec}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return time.Unix(0, 0), err
		}
		return minCtime, nil
	}

	someAtime := time.Date(2009, 5, 25, 9, 0, 0, 0, time.UTC)
	someMtime := time.Date(1984, 8, 10, 19, 15, 0, 0, time.UTC)

	t.Run("Dir", func(t *testing.T) {
		utils.MustMkdirAll(t, state.RootPath("dir"), 0755)

		wantMinCtime, err := chtimes(state.MountPath("dir"), someAtime, someMtime)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkTimes("dir", someAtime, someMtime, wantMinCtime); err != nil {
			t.Error(err)
		}
	})

	t.Run("File", func(t *testing.T) {
		utils.MustWriteFile(t, state.RootPath("file"), 0644, "new content")

		wantMinCtime, err := chtimes(state.MountPath("file"), someAtime, someMtime)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkTimes("file", someAtime, someMtime, wantMinCtime); err != nil {
			t.Error(err)
		}
	})

	t.Run("Symlink", func(t *testing.T) {
		utils.MustWriteFile(t, state.RootPath("target"), 0644, "")
		targetBefore, err := os.Lstat(state.MountPath("target"))
		if err != nil {
			t.Fatalf("Cannot stat target: %v", err)
		}

		utils.MustSymlink(t, "target", state.RootPath("symlink"))

		// Cope with the lack of utimensat on Travis macOS builds, just like we do in
		// build.rs.
		// TODO(https://github.com/bazelbuild/sandboxfs/issues/46): Remove this hack.
		if _, ok := os.LookupEnv("DO"); runtime.GOOS != "darwin" || !ok {
			wantMinCtime, err := chtimes(state.MountPath("symlink"), someAtime, someMtime)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkTimes("symlink", someAtime, someMtime, wantMinCtime); err != nil {
				t.Error(err)
			}
		} else {
			if _, err := chtimes(state.MountPath("symlink"), someAtime, someMtime); err == nil || err != syscall.EOPNOTSUPP {
				t.Fatalf("Expected EOPNOTSUPP changing the times of a symlink; got %v", err)
			}
		}
		targetAfter, err := os.Lstat(state.MountPath("target"))
		if err != nil {
			t.Fatalf("Cannot stat target: %v", err)
		}

		if !reflect.DeepEqual(targetBefore, targetAfter) {
			t.Errorf("Target file's times were unexpectedly modified: got %v, want %v", targetAfter, targetBefore)
		}
	})
}

func TestReadWrite_ChtimesResetsBirthtimeWithMtime(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	mustBtime := func(path string) time.Time {
		t.Helper()
		btime, err := utils.Btime(path)
		if err != nil {
			t.Fatalf("Cannot get birthtime for %s: %v", path, err)
			return btime // Not reached.
		}
		if btime == utils.ZeroBtime {
			t.Skipf("Birthtime not supported on this platform for %s", path)
			return btime // Not reached.
		}
		return btime
	}

	utils.MustWriteFile(t, state.RootPath("file"), 0644, "new content")
	path := state.MountPath("file")
	birth := mustBtime(path)

	// Let some time pass so that "now" becomes newer than the birth time.
	time.Sleep(time.Second)
	now := time.Now()

	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatalf("Failed to chtimes on %s: %v", path, err)
	}
	newBirth := mustBtime(path)
	if newBirth != birth {
		t.Errorf("Birthtime unexpectedly changed: got %v, want %v", newBirth, birth)
	}

	past := time.Date(1984, 8, 10, 19, 15, 0, 0, time.Local)
	if err := os.Chtimes(path, now, past); err != nil {
		t.Fatalf("Failed to chtimes on %s: %v", path, err)
	}
	newBirth = mustBtime(path)
	if newBirth != past {
		t.Errorf("Birthtime was not updated: got %v, want %v", newBirth, past)
	}
}

func TestReadWrite_FutimesOnDeletedNode(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.MountPath("dir"), 0755)
	utils.MustWriteFile(t, state.MountPath("file"), 0644, "")

	someAtime := time.Date(2010, 2, 20, 10, 30, 0, 0, time.UTC)
	someMtime := time.Date(1980, 3, 26, 12, 10, 0, 0, time.UTC)

	testData := []struct {
		name         string
		relativePath string
	}{
		{"MappedDir", "dir"},
		{"MappedFile", "file"},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			path := state.MountPath(d.relativePath)

			fd, err := openAndDelete(path, syscall.O_RDONLY)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Close(fd)

			tv := []syscall.Timeval{
				{Sec: int64(someAtime.Unix())},
				{Sec: int64(someMtime.Unix())},
			}
			if err := syscall.Futimes(fd, tv); err != nil {
				t.Fatalf("Fchown failed on deleted entry: %v", err)
			}

			var stat syscall.Stat_t
			if err := syscall.Fstat(fd, &stat); err != nil {
				t.Fatalf("Fstat failed on deleted entry: %v", err)
			}
			if !someAtime.Equal(utils.Atime(&stat)) || !someMtime.Equal(utils.Mtime(&stat)) {
				t.Errorf("Want atime %v, mtime %v; got atime %v, mtime %v", someAtime, someMtime, utils.Atime(&stat), utils.Mtime(&stat))
			}
		})
	}
}

func TestReadWrite_HardLinksNotSupported(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%", "--mapping=rw:/dir:%ROOT%/dir", "--mapping=rw:/scaffold/name3:%ROOT%/dir2")
	defer state.TearDown(t)

	utils.MustWriteFile(t, state.RootPath("name1"), 0644, "")
	utils.MustWriteFile(t, state.RootPath("dir/name2"), 0644, "")

	testData := []struct {
		name string

		dir       string // Directory on which to try the link operation.
		entryName string // Name of the entry to link.
	}{
		{"Root", "", "name1"},
		{"MappedDir", "dir", "name2"},
		{"ScaffoldDir", "scaffold", "name3"},
	}
	for _, d := range testData {
		t.Run(d.name, func(t *testing.T) {
			path := state.MountPath(d.dir, d.entryName)

			fileInfo, err := os.Lstat(path)
			if err != nil {
				t.Fatalf("Failed to stat %s before link attempt: %v", path, err)
			}
			wantNlink := fileInfo.Sys().(*syscall.Stat_t).Nlink

			if err := os.Link(path, state.MountPath(d.dir, "new-name")); !os.IsPermission(err) {
				t.Errorf("Want Link of %s to fail with permission error; got %v", path, err)
			}

			fileInfo, err = os.Lstat(path)
			if err != nil {
				t.Fatalf("Failed to stat %s after link attempt: %v", path, err)
			}
			stat := fileInfo.Sys().(*syscall.Stat_t)
			if stat.Nlink != wantNlink {
				t.Errorf("Want hard link count for %s to remain %d after failed link operation; got %d", path, wantNlink, stat.Nlink)
			}
		})
	}
}

func TestReadWrite_SymlinkAndReadlink(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	path := state.MountPath("symlink")
	target := "some/random/dangling/target"

	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("Failed to create symlink %s: %v", path, err)
	}

	gotTarget, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("Failed to read symlink %s: %v", path, err)
	}
	if target != gotTarget {
		t.Errorf("Want symlink target to be %s, got %s", target, gotTarget)
	}
}
func TestReadWrite_SymlinkAsDifferentUser(t *testing.T) {
	createAsDifferentUserTest(t, func(path string, user *utils.UnixUser) error {
		return utils.SymlinkAsUser("/non-existent/target", path, user)
	})
}

func TestReadWrite_MmapAfterMovesWorks(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	content := "some contents"
	utils.MustWriteFile(t, state.RootPath("name1"), 0644, content)
	utils.MustWriteFile(t, state.RootPath("name2"), 0644, "")

	readViaMmap := func(fd int) {
		data, err := syscall.Mmap(fd, 0, 8, syscall.PROT_READ, syscall.MAP_FILE|syscall.MAP_PRIVATE)
		if err != nil {
			t.Fatalf("Mmap failed: %v", err)
		}
		defer syscall.Munmap(data)

		// If sandboxfs confuses the kernel at any point during the file operations we
		// perform below, this read causes the test to crash.
		if dummy := data[0]; dummy != content[0] {
			t.Errorf("Got byte %b, want %b", dummy, content[0])
		}
	}

	path1 := state.MountPath("name1")
	path2 := state.MountPath("name2")

	fd, err := syscall.Open(path1, syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	readViaMmap(fd)
	syscall.Close(fd)

	fd, err = syscall.Open(path2, syscall.O_WRONLY|syscall.O_TRUNC, 0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if _, err := syscall.Write(fd, []byte(content)); err != nil {
		t.Fatalf("Cannot write: %v", err)
	}
	syscall.Close(fd)

	if err := os.Rename(path2, path1); err != nil {
		t.Fatalf("Rename failed: %v", err)
	}

	fd, err = syscall.Open(path1, syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	readViaMmap(fd)
	syscall.Close(fd)
}

func testXattrsOnDeletedFiles(t *testing.T, wantErr error, hook func(int) error) {
	state := utils.MountSetup(t, "--xattrs", "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)
	utils.MustWriteFile(t, state.RootPath("file"), 0644, "content")
	for _, name := range []string{"dir", "file"} {
		fd, err := openAndDelete(state.MountPath(name), syscall.O_RDONLY)
		if err != nil {
			t.Fatal(err)
		}
		defer syscall.Close(fd)

		if err := hook(fd); err != wantErr {
			t.Errorf("xattr operations via file handles not supported for %s; got %v, want %v", name, err, wantErr)
		}
	}
}

func TestReadWrite_Fgetxattr(t *testing.T) {
	testXattrsOnDeletedFiles(t, utils.MissingXattrErr, func(fd int) error {
		_, err := unix.Fgetxattr(fd, "user.foo", []byte{})
		return err
	})
}

func TestReadWrite_Flistxattr(t *testing.T) {
	testXattrsOnDeletedFiles(t, nil, func(fd int) error {
		_, err := unix.Flistxattr(fd, []byte{})
		return err
	})
}

func TestReadWrite_Setxattr(t *testing.T) {
	state := utils.MountSetup(t, "--xattrs", "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)
	utils.MustWriteFile(t, state.RootPath("file"), 0644, "new content")
	utils.MustSymlink(t, "missing", state.RootPath("symlink"))

	tests := []string{"dir", "file"}
	if runtime.GOOS != "linux" { // Linux doesn't support xattrs on symlinks.
		tests = append(tests, "symlink")
	}
	for _, name := range tests {
		wantValue := []byte("some-value")
		if err := unix.Lsetxattr(state.MountPath(name), "user.foo", wantValue, 0); err != nil {
			t.Fatalf("Lsetxattr(%s) failed: %v", name, err)
		}

		for _, path := range []string{state.MountPath(name), state.RootPath(name)} {
			buf := make([]byte, 32)
			sz, err := unix.Lgetxattr(path, "user.foo", buf)
			if err != nil {
				t.Fatalf("Listxattr(%s) failed: %v", path, err)
			}
			value := buf[0:sz]
			if !reflect.DeepEqual(value, wantValue) {
				t.Errorf("Invalid attribute for path %s: got %s, want %s", path, value, wantValue)
			}
		}
	}
}

func TestReadWrite_SetxattrOnScaffoldDirectory(t *testing.T) {
	state := utils.MountSetup(t, "--xattrs", "--mapping=rw:/:%ROOT%", "--mapping=rw:/scaffold/dir:%ROOT%")
	defer state.TearDown(t)

	path := state.MountPath("scaffold")
	value := []byte("some-value")
	wantErr := utils.WriteErrorForUnwritableNode()
	if err := unix.Lsetxattr(path, "user.foo", value, 0); err != wantErr {
		t.Errorf("Invalid error from Lsetxattr for %s: got %v, want %v", path, err, wantErr)
	}
}

func TestReadWrite_SetxattrDisabled(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=ro:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)

	var wantErr error
	switch runtime.GOOS {
	case "darwin":
		wantErr = unix.EPERM
	case "linux":
		wantErr = unix.EOPNOTSUPP
	default:
		panic("Don't know how this test behaves on this platform")
	}

	if err := unix.Lsetxattr(state.MountPath("dir"), "user.foo", []byte{}, 0); err != wantErr {
		t.Fatalf("Lsetxattr should have failed with %v, got %v", wantErr, err)
	}
}

func TestReadWrite_Fsetxattr(t *testing.T) {
	testXattrsOnDeletedFiles(t, unix.EACCES, func(fd int) error {
		return unix.Fsetxattr(fd, "user.foo", []byte{}, 0)
	})
}

func TestReadWrite_Removexattr(t *testing.T) {
	state := utils.MountSetup(t, "--xattrs", "--mapping=rw:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)
	utils.MustWriteFile(t, state.RootPath("file"), 0644, "new content")
	utils.MustSymlink(t, "missing", state.RootPath("symlink"))

	tests := []string{"dir", "file"}
	if runtime.GOOS != "linux" { // Linux doesn't support xattrs on symlinks.
		tests = append(tests, "symlink")
	}
	for _, name := range []string{"dir", "file"} {
		if err := unix.Lsetxattr(state.RootPath(name), "user.foo", []byte("some-value"), 0); err != nil {
			t.Fatalf("Lsetxattr(%s) failed: %v", name, err)
		}
		err := unix.Lremovexattr(state.MountPath(name), "user.foo")
		if err != nil {
			t.Fatalf("Lremovexattr(%s) failed: %v", name, err)
		}

		for _, path := range []string{state.MountPath(name), state.RootPath(name)} {
			buf := make([]byte, 32)
			if _, err := unix.Lgetxattr(path, "user.foo", buf); err == nil {
				t.Fatalf("Lgetxattr(%s) succeeded but want error", path)
			}
		}
	}
}

func TestReadWrite_RemovexattrOnScaffoldDirectory(t *testing.T) {
	state := utils.MountSetup(t, "--xattrs", "--mapping=rw:/:%ROOT%", "--mapping=rw:/scaffold/dir:%ROOT%")
	defer state.TearDown(t)

	path := state.MountPath("scaffold")
	wantErr := utils.WriteErrorForUnwritableNode()
	if err := unix.Lremovexattr(path, "user.foo"); err != wantErr {
		t.Errorf("Invalid error from Lremovexattr for %s: got %v, want %v", path, err, wantErr)
	}
}

func TestReadWrite_RemovexattrDisabled(t *testing.T) {
	state := utils.MountSetup(t, "--mapping=ro:/:%ROOT%")
	defer state.TearDown(t)

	utils.MustMkdirAll(t, state.RootPath("dir"), 0755)

	var wantErr error
	switch runtime.GOOS {
	case "darwin":
		wantErr = utils.MissingXattrErr
	case "linux":
		wantErr = unix.EOPNOTSUPP
	default:
		panic("Don't know how this test behaves on this platform")
	}

	if err := unix.Lremovexattr(state.MountPath("dir"), "user.foo"); err != wantErr {
		t.Fatalf("Lremovexattr should have failed with %v, got %v", wantErr, err)
	}
}

func TestReadWrite_Fremovexattr(t *testing.T) {
	testXattrsOnDeletedFiles(t, unix.EACCES, func(fd int) error {
		return unix.Fremovexattr(fd, "user.foo")
	})
}
