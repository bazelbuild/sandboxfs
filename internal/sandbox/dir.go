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

package sandbox

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
)

// Dir is a node that represents a directory backed by another directory that lives outside of
// the mount point.
//
// Dir object stores the children in three different maps. These act as layers and may contain
// nodes with the same key; in case of conflict, the one with the highest priority is preferred:
//
// mappedChildren: The mapped files/directories (given by user). These have the highest priority,
//     and will shadow any files/directories from the parent directory.
// baseChildren: The files and direcotries that are present in the underlying filesystem. This would
//     mean that either the parent directory or one of its ancestor is mapped to the filesystem.
// scaffoldDirs: Scaffold directories that are created by the sandbox to make it possible to
//     navigate to a nested mapping, if the intermediate directories do not exist in the
//       filesystem.
type Dir struct {
	BaseNode

	// mu synchronizes accesses to the directory entries.
	mu sync.Mutex

	// children contains the known entries of this directory.
	children map[string]Node
}

// openDir is a handle returned when a directory is opened.
//
// Note that the kernel can share open directory handles to serve different Readdir requests (even
// from different processes). In particular, this behavior was observed on macOS: if the directory
// is kept open while doing Readdir operations, those Readdir requests will reuse the same file
// handle. This poses difficulties because os.Readdir does not read from the beginning of the
// directory: we must make sure to rewind the handle before actually reading from it (and thus
// perform both operations exclusively). Failure to do so causes repeated Readdir operations to
// return incomplete data.
type openDir struct {
	// dir holds a pointer to the node from which this handle was opened.
	dir *Dir

	// mu synchronizes seeks and reads to the underlying directory so that we can always offer a
	// consistent view of its contents.
	mu sync.Mutex

	// nativeDir represents the OS-level open file handle for the directory. This is nil if the
	// open handle corresponds to a directory not backed by an underlying path.
	nativeDir *os.File

	// needRewind is true when we have already read the contents of the directory at least once.
	// We track this to avoid an unnecessary Seek system call on the file descriptor when the
	// descriptor is used exactly once, which is the vast majority of the cases.
	needRewind bool
}

var _ fs.Handle = (*openDir)(nil)

// newDirEmpty creates a new directory node to represent a fake directory that only contains
// mappings.
func newDirEmpty() *Dir {
	return &Dir{
		BaseNode: newUnmappedBaseNode(0555|os.ModeDir, 2),
		children: make(map[string]Node),
	}
}

// newDir creates a new directory node to represent the given underlying path.
//
// This function should never be called to explicitly create nodes. Instead, use the getOrCreateNode
// function, which respects the global node cache.
func newDir(path string, fileInfo os.FileInfo, writable bool) *Dir {
	return &Dir{
		BaseNode: newBaseNode(path, fileInfo, writable),
		children: make(map[string]Node),
	}
}

// LookupOrCreateDirs traverses a set of components and creates any that are missing as new empty
// directories.
//
// This function is intended to be used during (re)configuration to locate the directory from which
// to hang new mappings. Intermediate components created by this traversal are scaffold directories
// without a corresponding underlying directory: if the user wishes to map those to a real location
// on the file system, such mapping must be done before mapping anything else beneath them.
func (d *Dir) LookupOrCreateDirs(components []string) *Dir {
	if len(components) == 0 {
		return d
	}
	name := components[0]
	remainder := components[1:]

	d.mu.Lock()
	defer d.mu.Unlock()

	if child, ok := d.children[name]; ok {
		if dir, ok := child.(*Dir); ok {
			return dir.LookupOrCreateDirs(remainder)
		}
		return nil
	}
	child := newDirEmpty()
	child.isMapping = true
	d.children[name] = child
	return child.LookupOrCreateDirs(remainder)
}

// LookupOrFail traverses a set of components and returns the directory containing the final
// component, or nil if not found.
func (d *Dir) LookupOrFail(components []string) *Dir {
	if len(components) == 0 {
		return d
	}
	name := components[0]
	remainder := components[1:]

	d.mu.Lock()
	defer d.mu.Unlock()

	if child, ok := d.children[name]; ok {
		if dir, ok := child.(*Dir); ok {
			return dir.LookupOrFail(remainder)
		}
		return nil
	}
	return nil
}

// Map registers a new directory entry as a mapping to an arbitrary node.
//
// This function is intended to be used during (re)configuration to set up empty directories with
// the mappings specified by the user. The directory entry must not yet exist.
func (d *Dir) Map(name string, node Node) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.children[name]; ok {
		return fmt.Errorf("already mapped")
	}
	d.children[name] = node
	return nil
}

// Unmap unregisters a directory entry and all of its descendents.
//
// This function is intendd to be used during reconfigurations to delete parts of the tree that
// ought to disappear from the file system. It is OK to remap these same directory entries after
// a successful unmap.
func (d *Dir) Unmap(server *fs.Server, identity fs.Node, name string) error {
	d.mu.Lock()
	node, ok := d.children[name]
	if !ok {
		d.mu.Unlock()
		return fmt.Errorf("leaf %s not mapped", name)
	}
	if !node.IsMapping() {
		d.mu.Unlock()
		return fmt.Errorf("leaf %s not a mapping", name)
	}
	delete(d.children, name)
	d.mu.Unlock()
	server.InvalidateEntry(identity, name)
	return nil
}

// Open opens the file/directory in the underlying filesystem and returns a
// handle to it.
func (d *Dir) Open(_ context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	var openedDir *os.File
	if underlyingPath, isMapped := d.UnderlyingPath(); isMapped {
		var err error
		openedDir, err = os.OpenFile(underlyingPath, int(req.Flags), 0)
		if err != nil {
			return nil, fuseErrno(err)
		}
	}
	return &openDir{
		dir:       d,
		nativeDir: openedDir,
	}, nil
}

// lookupUnlocked returns the requested child node if present, or an error otherwise. This is a
// helper function and the caller is supposed to hold d.mu locked.
func (d *Dir) lookupUnlocked(name string) (fs.Node, error) {
	if child, ok := d.children[name]; ok {
		return child, nil
	}

	if underlyingPath, isMapped := d.UnderlyingPath(); isMapped {
		childPath := filepath.Join(underlyingPath, name)
		fileInfo, err := os.Lstat(childPath)
		if err != nil {
			return nil, fuseErrno(err)
		}

		child := getOrCreateNode(childPath, fileInfo, d.BaseNode.writable)
		d.children[name] = child
		return child, nil
	}

	// We don't know about the entry and we don't have a backing directory, so there is nothing
	// else we can return.
	return nil, fuse.ENOENT
}

// Lookup looks for a particular directory/file in all the children of a given
// directory.
func (d *Dir) Lookup(_ context.Context, name string) (fs.Node, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.lookupUnlocked(name)
}

// Dirent returns the directory entry corresponding to the directory.
func (d *Dir) Dirent(name string) fuse.Dirent {
	return fuse.Dirent{
		Inode: d.Inode(), // No need to lock: inode numbers are immutable.
		Name:  name,
		Type:  fuse.DT_Dir,
	}
}

// readEntries obtains the list of raw directory entries from the opened directory.
func (o *openDir) readEntries() ([]os.FileInfo, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.nativeDir == nil {
		return []os.FileInfo{}, nil
	}

	if o.needRewind {
		o.nativeDir.Seek(0, os.SEEK_SET)
	}
	var err error
	dirents, err := o.nativeDir.Readdir(-1)
	if err != nil {
		return nil, fuseErrno(err)
	}
	o.needRewind = true
	return dirents, nil

}

// ReadDirAll lists all files/directories inside a directory.
func (o *openDir) ReadDirAll(context.Context) ([]fuse.Dirent, error) {
	nativeDirents, err := o.readEntries()
	if err != nil {
		return nil, err
	}

	o.dir.mu.Lock()
	defer o.dir.mu.Unlock()

	dirents := make([]fuse.Dirent, 0, len(o.dir.children)+len(nativeDirents))
	done := make(map[string]bool)

	for name, node := range o.dir.children {
		if node.IsMapping() {
			dirents = append(dirents, node.Dirent(name))
			done[name] = true
		}
	}

	if len(nativeDirents) > 0 {
		underlyingPath, isMapped := o.dir.UnderlyingPath()
		if !isMapped {
			panic("Got some native dirents but we don't have an underlying path, and without an underlying path we cannot have issued a readdir")
		}

		for _, fileInfo := range nativeDirents {
			if _, ok := done[fileInfo.Name()]; ok {
				continue
			}

			path := filepath.Join(underlyingPath, fileInfo.Name())
			child := getOrCreateNode(path, fileInfo, o.dir.writable)
			o.dir.children[fileInfo.Name()] = child
			dirents = append(dirents, child.Dirent(fileInfo.Name()))
		}
	}
	return dirents, nil
}

// Release closes the underlying file/directory handle.
func (o *openDir) Release(_ context.Context, req *fuse.ReleaseRequest) error {
	if o.nativeDir == nil {
		return nil
	}
	return fuseErrno(o.nativeDir.Close())
}

// Link creates a hard link.
func (d *Dir) Link(ctx context.Context, req *fuse.LinkRequest, old fs.Node) (fs.Node, error) {
	return nil, fuseErrno(fuse.EPERM)
}

// Mkdir creates a new directory in the underlying file system.
func (d *Dir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.BaseNode.WantToWrite(); err != nil {
		return nil, err
	}

	underlyingPath, isMapped := d.UnderlyingPath()
	if !isMapped {
		return nil, fuse.EPERM
	}
	path := filepath.Join(underlyingPath, req.Name)

	if err := os.Mkdir(path, req.Mode&^req.Umask); err != nil {
		return nil, fuseErrno(err)
	}
	n, err := d.lookupUnlocked(req.Name)
	if err != nil {
		if e := os.Remove(path); e != nil {
			log.Printf("Deleting directory %q failed: %v", path, e)
		}
		return nil, fuseErrno(err)
	}
	return n, nil
}

// Mknod creates a new node (file, device, pipe etc) in the underlying
// directory.
func (d *Dir) Mknod(ctx context.Context, req *fuse.MknodRequest) (fs.Node, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.BaseNode.WantToWrite(); err != nil {
		return nil, err
	}

	underlyingPath, isMapped := d.UnderlyingPath()
	if !isMapped {
		return nil, fuse.EPERM
	}
	path := filepath.Join(underlyingPath, req.Name)

	err := unix.Mknod(
		path,
		UnixMode(req.Mode)&^uint32(req.Umask), // os.FileMode(same as uint32) -> uint32 (safe)
		int(req.Rdev),                         // uint32->int (safe)
	)
	if err != nil {
		return nil, fuseErrno(err)
	}
	n, err := d.lookupUnlocked(req.Name)
	if err != nil {
		if e := os.Remove(path); e != nil {
			log.Printf("Deleting node %q failed: %v", path, e)
		}
		return nil, fuseErrno(err)
	}
	return n, err
}

// Create creates a file in the underlying directory.
func (d *Dir) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.BaseNode.WantToWrite(); err != nil {
		return nil, nil, err
	}

	underlyingPath, isMapped := d.UnderlyingPath()
	if !isMapped {
		return nil, nil, fuse.EPERM
	}
	path := filepath.Join(underlyingPath, req.Name)

	openedFile, err := os.OpenFile(
		path,
		int(req.Flags), // uint32 -> int64
		req.Mode&^req.Umask,
	)
	if err != nil {
		return nil, nil, fuseErrno(err)
	}
	f, err := d.lookupUnlocked(req.Name)
	if err != nil {
		if e := os.Remove(path); e != nil {
			log.Printf("Deleting node %q failed: %v", path, e)
		}
		return nil, nil, fuseErrno(err)
	}
	file, ok := f.(*File)
	if !ok {
		// The file has been deleted (or replaced) between OpenFile and Lookup calls.
		return nil, nil, fuseErrno(syscall.EIO)
	}
	return f, &openFile{openedFile, file}, fuseErrno(err)
}

// Symlink creates a symlink in the underlying directory.
func (d *Dir) Symlink(ctx context.Context, req *fuse.SymlinkRequest) (fs.Node, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.BaseNode.WantToWrite(); err != nil {
		return nil, err
	}

	underlyingPath, isMapped := d.UnderlyingPath()
	if !isMapped {
		return nil, fuse.EPERM
	}
	path := filepath.Join(underlyingPath, req.NewName)

	err := os.Symlink(req.Target, path)
	if err != nil {
		return nil, fuseErrno(err)
	}
	n, err := d.lookupUnlocked(req.NewName)
	if err != nil {
		if e := os.Remove(path); e != nil {
			log.Printf("Deleting symlink %q failed: %v", path, e)
		}
		return nil, fuseErrno(err)
	}
	return n, err
}

// Rename renames a node or moves it to a different path.
func (d *Dir) Rename(ctx context.Context, req *fuse.RenameRequest, newDir fs.Node) error {
	resolve := func(dir *Dir, name string) (string, error) {
		if err := dir.BaseNode.WantToWrite(); err != nil {
			return "", err
		}

		// The child is guaranteed to be present in the source directory but may be missing
		// in the destination directory. Need to check for presence.
		if child, ok := dir.children[name]; ok {
			if child.IsMapping() {
				return "", fuse.EPERM
			}
		}

		underlyingPath, isMapped := dir.UnderlyingPath()
		if !isMapped {
			return "", fuse.EPERM
		}
		return filepath.Join(underlyingPath, name), nil
	}

	nd, ok := newDir.(*Dir)
	if !ok {
		return fuseErrno(syscall.ENOTDIR)
	}

	// Ensure lock ordering to prevent deadlocks.
	first, second := d, nd
	if first.Inode() > second.Inode() {
		first, second = second, first
	}
	first.mu.Lock()
	defer first.mu.Unlock()
	if first != second {
		second.mu.Lock()
		defer second.mu.Unlock()
	}

	oldPath, err := resolve(d, req.OldName)
	if err != nil {
		return err
	}
	newPath, err := resolve(nd, req.NewName)
	if err != nil {
		return err
	}
	err = os.Rename(oldPath, newPath)
	if err != nil {
		return fuseErrno(err)
	}

	child := d.children[req.OldName]
	delete(d.children, req.OldName)
	child.SetUnderlyingPath(newPath)
	nd.children[req.NewName] = child

	return nil
}

// Remove unlinks a node from the underlying directory.
func (d *Dir) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.BaseNode.WantToWrite(); err != nil {
		return err
	}

	underlyingPath, isMapped := d.UnderlyingPath()
	if !isMapped {
		return fuse.EPERM
	}
	path := filepath.Join(underlyingPath, req.Name)

	if child, ok := d.children[req.Name]; ok {
		if child.IsMapping() {
			return fuse.EPERM
		}
	}

	err := os.Remove(path)
	if err == nil {
		d.children[req.Name].delete()
		delete(d.children, req.Name)
	}
	return fuseErrno(err)
}
