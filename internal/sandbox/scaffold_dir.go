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

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

// ScaffoldDir is a node type that represents a scaffold directory in the file system. Scaffold
// directories are in-memory, read-only directories that back the path components in nested mappings
// that have no corresponding physical directory.
//
// ScaffoldDir stores the children in two different maps. These act as layers and may contain nodes
// with the same key; in case of conflict, the one with the highest priority is preferred:
//
// mappedChildren: The mapped files/directories (given by user). These have the highest priority,
//     and will shadow any scaffold directories from the parent directory.
// scaffoldDirs: Scaffold directories that are created by the sandbox to make it possible to
//     navigate to a nested mapping, if the intermediate directories do not exist in the filesystem.
type ScaffoldDir struct {
	inode          uint64
	mappedChildren map[string]Node
	scaffoldDirs   map[string]*ScaffoldDir
}

// OpenScaffoldDir is the handle corresponding to an opened ScaffoldDir node.
type OpenScaffoldDir struct {
	nativeScaffoldDir *ScaffoldDir
}

// newScaffoldDir initializes a new scaffold directory node with the proper inode
// number.
func newScaffoldDir() *ScaffoldDir {
	return &ScaffoldDir{
		inode:          nextInodeNumber(),
		mappedChildren: make(map[string]Node),
		scaffoldDirs:   make(map[string]*ScaffoldDir),
	}
}

// Attr populates 'a' with the file/directory metadata.
func (v *ScaffoldDir) Attr(_ context.Context, a *fuse.Attr) error {
	a.Inode = v.inode
	a.Mode = 0555 | os.ModeDir
	a.Valid = attrValidTime

	// Directories need to have their link count explicitly set to 2 (and no more than 2 because
	// we don't support hard links on directories) to represent the "." and ".."  names. FUSE
	// handles those two names internally which means that we never get called back to handle
	// their "creation".
	a.Nlink = 2

	return nil
}

// Open opens the file/directory in the underlying filesystem and returns a
// handle to it.
func (v *ScaffoldDir) Open(context.Context, *fuse.OpenRequest, *fuse.OpenResponse) (fs.Handle, error) {
	return &OpenScaffoldDir{v}, nil
}

// Lookup looks for a particular directory/file in all the children of a given
// directory.
func (v *ScaffoldDir) Lookup(_ context.Context, name string) (fs.Node, error) {
	return v.lookup(name)
}

// ReadDirAll lists all files/directories inside a directory.
func (o *OpenScaffoldDir) ReadDirAll(context.Context) ([]fuse.Dirent, error) {
	done := make(map[string]bool)
	dirEntries := make([]fuse.Dirent, 0, len(o.nativeScaffoldDir.mappedChildren)+len(o.nativeScaffoldDir.scaffoldDirs))

	for name, node := range o.nativeScaffoldDir.mappedChildren {
		done[name] = true
		dirEntries = append(dirEntries, node.Dirent(name))
	}
	for name, node := range o.nativeScaffoldDir.scaffoldDirs {
		if !done[name] {
			done[name] = true
			dirEntries = append(dirEntries, node.Dirent(name))
		}
	}
	return dirEntries, nil
}

// Inode returns the node's inode number.
func (v *ScaffoldDir) Inode() uint64 {
	return v.inode
}

// Dirent returns the directory entry corresponding to the scaffold directory.
func (v *ScaffoldDir) Dirent(name string) fuse.Dirent {
	return fuse.Dirent{
		Inode: v.Inode(),
		Name:  name,
		Type:  fuse.DT_Dir,
	}
}

// EquivalentDir returns a *Dir instance that shares its children with the ScaffoldDir
// instance.
func (v *ScaffoldDir) EquivalentDir(path string, fileInfo os.FileInfo, writable bool) *MappedDir {
	return newMappedDirFromExisting(v.mappedChildren, v.scaffoldDirs, path, fileInfo, writable)
}

// scaffoldDirChild returns a ScaffoldDir child for the current scaffoldDir,
// creating it if it doesn't exist.
//
// Note that this function isn't thread safe, and relies on the calling
// function to ensure thread safety.
func (v *ScaffoldDir) scaffoldDirChild(name string) *ScaffoldDir {
	if _, ok := v.scaffoldDirs[name]; !ok {
		v.scaffoldDirs[name] = newScaffoldDir()
	}
	return v.scaffoldDirs[name]
}

// newNodeChild creates and returns a child for the current scaffoldDir.
// If the child already exists, it returns an error, regardless of whether the
// existing child has the same underlying path or not.
//
// Note that this function isn't thread safe, and relies on the calling
// function to ensure thread safety.
func (v *ScaffoldDir) newNodeChild(name, target string, writable bool) (Node, error) {
	if _, ok := v.mappedChildren[name]; ok {
		return nil, fmt.Errorf("two nodes mapped at the same location")
	}

	fileInfo, err := os.Lstat(target)
	if err != nil {
		return nil, fmt.Errorf("creating node for path %q failed: %v", target, err)
	}

	if scaffold, ok := v.scaffoldDirs[name]; ok {
		if !fileInfo.IsDir() {
			return nil, fmt.Errorf("file %q mapped over existing directory", target)
		}
		v.mappedChildren[name] = scaffold.EquivalentDir(target, fileInfo, writable)
	} else {
		v.mappedChildren[name] = newNodeForFileInfo(target, fileInfo, writable)
	}
	return v.mappedChildren[name], nil
}

// lookup checks and returns a child of ScaffoldDir if found.
func (v *ScaffoldDir) lookup(name string) (fs.Node, error) {
	if child, ok := v.mappedChildren[name]; ok {
		return child, nil
	}
	if child, ok := v.scaffoldDirs[name]; ok {
		return child, nil
	}
	return nil, fuseErrno(syscall.ENOENT)
}

// invalidate clears the kernel cache for this directory and all of its entries.
func (v *ScaffoldDir) invalidate(server *fs.Server) {
	err := server.InvalidateNodeData(v)
	logCacheInvalidationError(err, "Could not invalidate node cache: ", v)

	v.invalidateEntries(server, v)
}

// invalidateEntries clears the kernel cache for all entries that descend from this directory.
//
// The identity parameter indicates who the real owner of the entries is from the point of view of
// the FUSE API. In the general case, identity will match v, but in the case of the root directory,
// identity will be that of the Root node.
func (v *ScaffoldDir) invalidateEntries(server *fs.Server, identity fs.Node) {
	invalidate := func(name string, node cacheInvalidator) {
		err := server.InvalidateEntry(identity, name)
		logCacheInvalidationError(err, "Could not invalidate node entry: ", identity, name)
		node.invalidate(server)
	}

	for name, node := range v.mappedChildren {
		invalidate(name, node)
	}
	for name, node := range v.scaffoldDirs {
		invalidate(name, node)
	}
}

// Create creates a new file in the given directory.
func (v *ScaffoldDir) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	return nil, nil, fuse.EPERM
}

// Link creates a hard link.
func (v *ScaffoldDir) Link(ctx context.Context, req *fuse.LinkRequest, old fs.Node) (fs.Node, error) {
	return nil, fuseErrno(fuse.EPERM)
}

// Mkdir creates a new directory in the given directory.
func (v *ScaffoldDir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	return nil, fuse.EPERM
}

// Mknod creates a new node in the given directory.
func (v *ScaffoldDir) Mknod(ctx context.Context, req *fuse.MknodRequest) (fs.Node, error) {
	return nil, fuse.EPERM
}

// Remove unlinks a node in the directory.
func (v *ScaffoldDir) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	return fuse.EPERM
}

// Rename renames/moves a node from one directory to another.
func (v *ScaffoldDir) Rename(ctx context.Context, req *fuse.RenameRequest, newDir fs.Node) error {
	return fuse.EPERM
}

// Setattr updates the directory metadata.
func (v *ScaffoldDir) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	return fuse.EPERM
}

// Symlink creates a new symlink in the given directory.
func (v *ScaffoldDir) Symlink(ctx context.Context, req *fuse.SymlinkRequest) (fs.Node, error) {
	return nil, fuse.EPERM
}
