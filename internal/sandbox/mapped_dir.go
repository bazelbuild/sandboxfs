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

// MappedDir is a node that represents a directory backed by another directory that lives outside of
// the mount point.
//
// MappedDir object stores the children in three different maps. These act as layers and may contain
// nodes with the same key; in case of conflict, the one with the highest priority is preferred:
//
// mappedChildren: The mapped files/directories (given by user). These have the highest priority,
//     and will shadow any files/directories from the parent directory.
// baseChildren: The files and direcotries that are present in the underlying filesystem. This would
//     mean that either the parent directory or one of its ancestor is mapped to the filesystem.
// scaffoldDirs: Scaffold directories that are created by the sandbox to make it possible to
//     navigate to a nested mapping, if the intermediate directories do not exist in the
//       filesystem.
type MappedDir struct {
	BaseNode

	mu             sync.Mutex // mu protects the maps below.
	mappedChildren map[string]Node
	baseChildren   map[string]Node
	scaffoldDirs   map[string]*ScaffoldDir
}

// openMappedDir is a handle returned when a directory is opened.
//
// Note that the kernel can share open directory handles to serve different Readdir requests (even
// from different processes). In particular, this behavior was observed on macOS: if the directory
// is kept open while doing Readdir operations, those Readdir requests will reuse the same file
// handle. This poses difficulties because os.Readdir does not read from the beginning of the
// directory: we must make sure to rewind the handle before actually reading from it (and thus
// perform both operations exclusively). Failure to do so causes repeated Readdir operations to
// return incomplete data.
type openMappedDir struct {
	// dir holds a pointer to the node from which this handle was opened.
	dir *MappedDir

	// mu synchronizes seeks and reads to the underlying directory so that we can always offer a
	// consistent view of its contents.
	mu sync.Mutex

	// nativeDir represents the OS-level open file handle for the directory.
	nativeDir *os.File

	// needRewind is true when we have already read the contents of the directory at least once.
	// We track this to avoid an unnecessary Seek system call on the file descriptor when the
	// descriptor is used exactly once, which is the vast majority of the cases.
	needRewind bool
}

var _ fs.Handle = (*openMappedDir)(nil)

// newMappedDir initializes a new directory node with the proper inode number.
func newMappedDir(path string, id DevInoPair, writable bool) *MappedDir {
	return &MappedDir{
		BaseNode:       newBaseNode(path, id, writable),
		mappedChildren: make(map[string]Node),
		baseChildren:   make(map[string]Node),
		scaffoldDirs:   make(map[string]*ScaffoldDir),
	}
}

// newMappedDirFromExisting returns a *MappedDir instance that shares scaffoldDirs and
// mappedChildren with an existing node.
func newMappedDirFromExisting(mappedChildren map[string]Node,
	scaffoldDirs map[string]*ScaffoldDir, path string, id DevInoPair, writable bool) *MappedDir {
	return &MappedDir{
		BaseNode:       newBaseNode(path, id, writable),
		mappedChildren: mappedChildren,
		baseChildren:   make(map[string]Node),
		scaffoldDirs:   scaffoldDirs,
	}
}

// Open opens the file/directory in the underlying filesystem and returns a
// handle to it.
func (d *MappedDir) Open(_ context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	openedDir, err := os.OpenFile(d.underlyingPath, int(req.Flags), 0)
	if err != nil {
		return nil, fuseErrno(err)
	}
	return &openMappedDir{
		dir:       d,
		nativeDir: openedDir,
	}, nil
}

// Setattr updates the directory metadata.
func (d *MappedDir) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	_, err := d.BaseNode.Setattr(ctx, req)
	return err
}

// lookup looks for a particular node in all the children of d.
// NOTE: lookup assumes that the caller function does not hold lock mu on MappedDir.
func (d *MappedDir) lookup(name string) (fs.Node, error) {
	// TODO(pallavag): Ideally, we should not have to call lookup from
	// functions other than fuse Lookup just to get a reference to the node.
	// However, we need *at functions (fstatat for example) to get attr values
	// for node without having to use stat syscall again (from open file
	// pointer, for instance). These are not available in Go unix library yet.
	//
	// We need to stat because we don't know if underlying FS could have
	// changed since last time (when we cached the node object).
	//
	// We could avoid stat until we have checked mappedChildren, but we don't
	// want to hold the mutex for a system call.
	fileInfo, statErr := os.Lstat(filepath.Join(d.underlyingPath, name))

	d.mu.Lock()
	defer d.mu.Unlock()
	if mappedChild, ok := d.mappedChildren[name]; ok {
		return mappedChild, nil
	}
	scaffoldChild, scaffoldOK := d.scaffoldDirs[name]
	if statErr != nil {
		// If the underlying node is unaccessible due to any reason, we do not
		// want the rest of the mappings (nested ones for instance) to be
		// affected. Thus, switch to scaffold directory if present.
		if scaffoldOK {
			return scaffoldChild, nil
		}
		return nil, fuseErrno(statErr)
	}

	if child := d.baseChildFromFileInfo(fileInfo); child != nil {
		d.baseChildren[name] = child
		return child, nil
	}
	return scaffoldChild, nil
}

// Lookup looks for a particular directory/file in all the children of a given
// directory.
func (d *MappedDir) Lookup(_ context.Context, name string) (fs.Node, error) {
	return d.lookup(name)
}

// Dirent returns the directory entry corresponding to the directory.
func (d *MappedDir) Dirent(name string) fuse.Dirent {
	return fuse.Dirent{
		Inode: d.Inode(),
		Name:  name,
		Type:  fuse.DT_Dir,
	}
}

// ReadDirAll lists all files/directories inside a directory.
func (o *openMappedDir) ReadDirAll(context.Context) ([]fuse.Dirent, error) {
	o.mu.Lock()
	if o.needRewind {
		o.nativeDir.Seek(0, os.SEEK_SET)
	}
	dirents, err := o.nativeDir.Readdir(-1)
	if err != nil {
		o.mu.Unlock()
		return nil, fuseErrno(err)
	}
	o.needRewind = true
	o.mu.Unlock()

	done := make(map[string]bool)

	o.dir.mu.Lock()
	defer o.dir.mu.Unlock()
	dirEntries := make([]fuse.Dirent, 0, len(o.dir.mappedChildren)+len(dirents)+len(o.dir.scaffoldDirs))
	for name, node := range o.dir.mappedChildren {
		dirEntries = append(dirEntries, node.Dirent(name))
		done[name] = true
	}
	for _, fileInfo := range dirents {
		if done[fileInfo.Name()] {
			continue
		}
		if child := o.dir.baseChildFromFileInfo(fileInfo); child != nil {
			o.dir.baseChildren[fileInfo.Name()] = child
			dirEntries = append(dirEntries, child.Dirent(fileInfo.Name()))
			done[fileInfo.Name()] = true
		}
	}
	for name, node := range o.dir.scaffoldDirs {
		if !done[name] {
			dirEntries = append(dirEntries, node.Dirent(name))
			done[name] = true
		}
	}
	return dirEntries, nil
}

// Release closes the underlying file/directory handle.
func (o *openMappedDir) Release(_ context.Context, req *fuse.ReleaseRequest) error {
	return fuseErrno(o.nativeDir.Close())
}

// Link creates a hard link.
func (d *MappedDir) Link(ctx context.Context, req *fuse.LinkRequest, old fs.Node) (fs.Node, error) {
	return nil, fuseErrno(fuse.EPERM)
}

// Mkdir creates a new directory in the underlying file system.
func (d *MappedDir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	if err := d.BaseNode.WantToWrite(); err != nil {
		return nil, err
	}

	path := filepath.Join(d.underlyingPath, req.Name)
	if err := os.Mkdir(path, req.Mode&^req.Umask); err != nil {
		return nil, fuseErrno(err)
	}
	n, err := d.lookup(req.Name)
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
func (d *MappedDir) Mknod(ctx context.Context, req *fuse.MknodRequest) (fs.Node, error) {
	if err := d.BaseNode.WantToWrite(); err != nil {
		return nil, err
	}

	path := filepath.Join(d.underlyingPath, req.Name)
	err := unix.Mknod(
		path,
		UnixMode(req.Mode)&^uint32(req.Umask), // os.FileMode(same as uint32) -> uint32 (safe)
		int(req.Rdev),                         // uint32->int (safe)
	)
	if err != nil {
		return nil, fuseErrno(err)
	}
	n, err := d.lookup(req.Name)
	if err != nil {
		if e := os.Remove(path); e != nil {
			log.Printf("Deleting node %q failed: %v", path, e)
		}
		return nil, fuseErrno(err)
	}
	return n, err
}

// Create creates a file in the underlying directory.
func (d *MappedDir) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	if err := d.BaseNode.WantToWrite(); err != nil {
		return nil, nil, err
	}

	path := filepath.Join(d.underlyingPath, req.Name)
	openedFile, err := os.OpenFile(
		path,
		int(req.Flags), // uint32 -> int64
		req.Mode&^req.Umask,
	)
	if err != nil {
		return nil, nil, fuseErrno(err)
	}
	f, err := d.lookup(req.Name)
	if err != nil {
		if e := os.Remove(path); e != nil {
			log.Printf("Deleting node %q failed: %v", path, e)
		}
		return nil, nil, fuseErrno(err)
	}
	file, ok := f.(*MappedFile)
	if !ok {
		// The file has been deleted (or replaced) between OpenFile and Lookup calls.
		return nil, nil, fuseErrno(syscall.EIO)
	}
	return f, &openMappedFile{openedFile, file}, fuseErrno(err)
}

// Symlink creates a symlink in the underlying directory.
func (d *MappedDir) Symlink(ctx context.Context, req *fuse.SymlinkRequest) (fs.Node, error) {
	if err := d.BaseNode.WantToWrite(); err != nil {
		return nil, err
	}

	path := filepath.Join(d.underlyingPath, req.NewName)
	err := os.Symlink(req.Target, path)
	if err != nil {
		return nil, fuseErrno(err)
	}
	n, err := d.lookup(req.NewName)
	if err != nil {
		if e := os.Remove(path); e != nil {
			log.Printf("Deleting symlink %q failed: %v", path, e)
		}
		return nil, fuseErrno(err)
	}
	return n, err
}

// Rename renames a node or moves it to a different path.
func (d *MappedDir) Rename(ctx context.Context, req *fuse.RenameRequest, newDir fs.Node) error {
	if err := d.BaseNode.WantToWrite(); err != nil {
		return err
	}

	nd, ok := newDir.(*MappedDir)
	if !ok {
		if _, ok := newDir.(*ScaffoldDir); ok {
			return fuseErrno(syscall.EPERM)
		}
		return fuseErrno(syscall.ENOTDIR)
	}

	if err := nd.BaseNode.WantToWrite(); err != nil {
		return err
	}

	err := os.Rename(filepath.Join(d.underlyingPath, req.OldName), filepath.Join(nd.underlyingPath, req.NewName))
	if err != nil {
		return fuseErrno(err)
	}

	// Ensure lock ordering to prevent deadlocks.
	first, second := d, nd
	if first.inode > second.inode {
		first, second = second, first
	}
	first.mu.Lock()
	defer first.mu.Unlock()
	if first != second {
		second.mu.Lock()
		defer second.mu.Unlock()
	}

	// If the node has been used before, kernel already expects a particular
	// inode number. So we shift the node into the new directory's map.
	if child, ok := d.baseChildren[req.OldName]; ok {
		child.SetUnderlyingPath(filepath.Join(nd.underlyingPath, req.NewName))
		delete(d.baseChildren, req.OldName)
		nd.baseChildren[req.NewName] = child
	}
	// If the node hasn't been used before, then ok = false, and we do not need
	// to do anything to change our internal states.
	return nil
}

// Remove unlinks a node from the underlying directory.
func (d *MappedDir) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	if err := d.BaseNode.WantToWrite(); err != nil {
		return err
	}

	return fuseErrno(os.Remove(filepath.Join(d.underlyingPath, req.Name)))
}

// childForNodeType intializes a new child node based on the type in mode.
func childForNodeType(underlyingPath, name string, id DevInoPair, mode os.FileMode, writable bool) Node {
	switch mode & os.ModeType {
	case os.ModeDir:
		return newMappedDir(filepath.Join(underlyingPath, name), id, writable)
	case os.ModeSymlink:
		return newMappedSymlink(filepath.Join(underlyingPath, name), id, writable)
	default:
		// Everything else behaves like a regular file because there are no
		// FUSE-specific operations to be implemented for them.
		return newMappedFile(filepath.Join(underlyingPath, name), id, writable)
	}
}

// baseChildFromFileInfo returns a node corresponding to underlying node if
// permitted by its priority level, nil otherwise.
//
// This assumes that d.mu has been locked by the caller.
func (d *MappedDir) baseChildFromFileInfo(fileInfo os.FileInfo) Node {
	name := fileInfo.Name()
	baseChild, baseOK := d.baseChildren[name]
	scaffoldChild, scaffoldOK := d.scaffoldDirs[name]
	id := fileInfoToID(fileInfo)

	if !scaffoldOK {
		// We need to check if the node is still same type as before, since we
		// don't want to create a new node (and hence a new indode number) if it is.
		if !baseOK || baseChild.UnderlyingID() != id {
			baseChild = childForNodeType(d.underlyingPath, name, id, fileInfo.Mode(), d.BaseNode.writable)
		}
	} else {
		// Control reaches here if there's an entry in scaffoldDirs, as well as
		// a node exists in underlying filesystem.
		// scaffoldDirs takes preference if the underlying node is a file (and
		// thus we return nil).
		if !fileInfo.IsDir() {
			return nil
		}
		if !baseOK || baseChild.UnderlyingID() != id {
			baseChild = scaffoldChild.EquivalentDir(filepath.Join(d.underlyingPath, name), id, d.BaseNode.writable)
		}
	}
	return baseChild
}

// invalidate clears the kernel cache for this directory and all of its entries.
func (d *MappedDir) invalidate(server *fs.Server) {
	err := server.InvalidateNodeData(d)
	logCacheInvalidationError(err, "Could not invalidate node cache: ", d)

	d.invalidateEntries(server, d)
}

// invalidateEntries clears the kernel cache for all entries that descend from this directory.
//
// The identity parameter indicates who the real owner of the entries is from the point of view of
// the FUSE API. In the general case, identity will match v, but in the case of the root directory,
// identity will be that of the Root node.
func (d *MappedDir) invalidateEntries(server *fs.Server, identity fs.Node) {
	d.mu.Lock()
	entries := make(map[string]cacheInvalidator)
	for name, node := range d.mappedChildren {
		entries[name] = node
	}
	for name, node := range d.baseChildren {
		entries[name] = node
	}
	for name, node := range d.scaffoldDirs {
		entries[name] = node
	}
	d.mu.Unlock()

	for name, node := range entries {
		err := server.InvalidateEntry(identity, name)
		logCacheInvalidationError(err, "Could not invalidate node entry: ", identity, name)
		node.invalidate(server)
	}
}
