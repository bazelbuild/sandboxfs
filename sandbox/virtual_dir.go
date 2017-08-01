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
	"os"
	"syscall"

	"golang.org/x/net/context"
	"bazil.org/fuse/fs"
	"bazil.org/fuse"
)

// VirtualDir is a node type that represents a virtual directory in the file
// system. Virtual directories are needed to back the path components in nested
// mappings that have no corresponding physical directory.
//
// VirtualDir stores the children in two different maps. These act as layers
// and may contain nodes with the same key; in case of conflict, the one with
// the highest priority is preferred:
//
//   mappedChildren: The mapped files/directories (given by user).
//                   These have the highest priority, and will shadow any
//                   virtual directories from the parent directory.
//   virtualDirs:    Virtual directories that are created by the sandbox to
//                   make it possible to navigate to a nested mapping, if the
//                   intermediate directories do not exist in the
//                   filesystem.
type VirtualDir struct {
	inode          uint64
	mappedChildren map[string]Node
	virtualDirs    map[string]*VirtualDir
}

// OpenVirtualDir is the handle corresponding to an opened VirtualDir node.
type OpenVirtualDir struct {
	nativeVirtualDir *VirtualDir
}

// newVirtualDir initializes a new virtual directory node with the proper inode
// number.
func newVirtualDir() *VirtualDir {
	return &VirtualDir{
		inode:          nextInodeNumber(),
		mappedChildren: make(map[string]Node),
		virtualDirs:    make(map[string]*VirtualDir),
	}
}

// Attr populates 'a' with the file/directory metadata.
func (v *VirtualDir) Attr(_ context.Context, a *fuse.Attr) error {
	a.Inode = v.inode
	a.Mode = 0555 | os.ModeDir
	return nil
}

// Open opens the file/directory in the underlying filesystem and returns a
// handle to it.
func (v *VirtualDir) Open(context.Context, *fuse.OpenRequest, *fuse.OpenResponse) (fs.Handle, error) {
	return &OpenVirtualDir{v}, nil
}

// Access checks for permissions on a given node in the file system.
func (v *VirtualDir) Access(_ context.Context, req *fuse.AccessRequest) error {
	if req.Mask&2 != 0 { // W_OK permission == 2
		return fuseErrno(syscall.EACCES)
	}
	return nil
}

// Lookup looks for a particular directory/file in all the children of a given
// directory.
func (v *VirtualDir) Lookup(_ context.Context, name string) (fs.Node, error) {
	return v.lookup(name)
}

// ReadDirAll lists all files/directories inside a directory.
func (o *OpenVirtualDir) ReadDirAll(context.Context) ([]fuse.Dirent, error) {
	done := make(map[string]bool)
	dirEntries := make([]fuse.Dirent, 0, len(o.nativeVirtualDir.mappedChildren)+len(o.nativeVirtualDir.virtualDirs))

	for name, node := range o.nativeVirtualDir.mappedChildren {
		done[name] = true
		dirEntries = append(dirEntries, node.Dirent(name))
	}
	for name, node := range o.nativeVirtualDir.virtualDirs {
		if !done[name] {
			done[name] = true
			dirEntries = append(dirEntries, node.Dirent(name))
		}
	}
	return dirEntries, nil
}

// Inode returns the node's inode number.
func (v *VirtualDir) Inode() uint64 {
	return v.inode
}

// Dirent returns the directory entry corresponding to the virtual directory.
func (v *VirtualDir) Dirent(name string) fuse.Dirent {
	return fuse.Dirent{
		Inode: v.Inode(),
		Name:  name,
		Type:  fuse.DT_Dir,
	}
}

// EquivalentDir returns a *Dir instance that shares its children with the VirtualDir
// instance.
func (v *VirtualDir) EquivalentDir(path string, id DevInoPair, writable bool) *Dir {
	return newDirFromExisting(v.mappedChildren, v.virtualDirs, path, id, writable)
}

// virtualDirChild returns a VirtualDir child for the current virtualDir,
// creating it if it doesn't exist.
//
// Note that this function isn't thread safe, and relies on the calling
// function to ensure thread safety.
func (v *VirtualDir) virtualDirChild(name string) *VirtualDir {
	if _, ok := v.virtualDirs[name]; !ok {
		v.virtualDirs[name] = newVirtualDir()
	}
	return v.virtualDirs[name]
}

// newNodeChild creates and returns a child for the current virtualDir.
// If the child already exists, it returns an error, regardless of whether the
// existing child has the same underlying path or not.
//
// Note that this function isn't thread safe, and relies on the calling
// function to ensure thread safety.
func (v *VirtualDir) newNodeChild(name, target string, writable bool) (Node, error) {
	if _, ok := v.mappedChildren[name]; ok {
		return nil, fmt.Errorf("two nodes mapped at the same location")
	}

	fileInfo, err := os.Lstat(target)
	if err != nil {
		return nil, fmt.Errorf("creating node for path %q failed: %v", target, err)
	}
	id := fileInfoToID(fileInfo)

	if virtual, ok := v.virtualDirs[name]; ok {
		if !fileInfo.IsDir() {
			return nil, fmt.Errorf("file %q mapped over existing directory", target)
		}
		v.mappedChildren[name] = virtual.EquivalentDir(target, id, writable)
	} else {
		v.mappedChildren[name] = newNodeForFileInfo(fileInfo, target, id, writable)
	}
	return v.mappedChildren[name], nil
}

// lookup checks and returns a child of VirtualDir if found.
func (v *VirtualDir) lookup(name string) (fs.Node, error) {
	if child, ok := v.mappedChildren[name]; ok {
		return child, nil
	}
	if child, ok := v.virtualDirs[name]; ok {
		return child, nil
	}
	return nil, fuseErrno(syscall.ENOENT)
}

// invalidateRecursively clears the kernel cache corresponding to this node,
// and children if present.
func (v *VirtualDir) invalidateRecursively(server *fs.Server) {
	v.invalidateRecursivelyParent(v, server)
}

// invalidateRecursivelyParent calls cache invalidations, using a different
// parent for the entry invalidations (since other types like Root may wrap
// over VirtualDir, and would be the true parent as far as the kernel is
// concerned).
func (v *VirtualDir) invalidateRecursivelyParent(parent fs.Node, server *fs.Server) {
	err := server.InvalidateNodeData(v)
	logCacheInvalidationError(err, "Could not invalidate node cache: ", v)

	invalidate := func(name string, node cacheInvalidator) {
		err := server.InvalidateEntry(parent, name)
		logCacheInvalidationError(err, "Could not invalidate node entry: ", parent, name)
		node.invalidateRecursively(server)
	}

	for name, node := range v.mappedChildren {
		invalidate(name, node)
	}
	for name, node := range v.virtualDirs {
		invalidate(name, node)
	}
}

// Create creates a new file in the given directory.
func (v *VirtualDir) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	return nil, nil, fuse.EPERM
}

// Mkdir creates a new directory in the given directory.
func (v *VirtualDir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	return nil, fuse.EPERM
}

// Mknod creates a new node in the given directory.
func (v *VirtualDir) Mknod(ctx context.Context, req *fuse.MknodRequest) (fs.Node, error) {
	return nil, fuse.EPERM
}

// Remove unlinks a node in the directory.
func (v *VirtualDir) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	return fuse.EPERM
}

// Rename renames/moves a node from one directory to another.
func (v *VirtualDir) Rename(ctx context.Context, req *fuse.RenameRequest, newDir fs.Node) error {
	return fuse.EPERM
}

// Symlink creates a new symlink in the given directory.
func (v *VirtualDir) Symlink(ctx context.Context, req *fuse.SymlinkRequest) (fs.Node, error) {
	return nil, fuse.EPERM
}
