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
	"math"
	"os"
	"syscall"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

// BaseNode is a common type for all nodes: files, directories, pipes, symlinks, etc.
type BaseNode struct {
	inode          uint64
	underlyingPath string
	underlyingID   DevInoPair
	writable       bool
}

// Node defines the properties common to every node in the filesystem tree.
type Node interface {
	fs.Node

	// Inode returns the inode number for the given node.
	Inode() uint64

	// UnderlyingID returns the pair {device number, inode} for the
	// file/directory corresponding to the node in the underlying filesystem.
	UnderlyingID() DevInoPair

	// Dirent returns the directory entry for the node on which it is
	// called.
	// The node's name is the basename of the directory entry (no path components),
	// and needs to be passed in because it is not stored within the node itself.
	Dirent(name string) fuse.Dirent

	// SetUnderlyingPath changes the Node's underlying path to the specified
	// value.
	SetUnderlyingPath(path string)

	// UnderlyingPath returns the Node's path in the underlying filesystem.
	UnderlyingPath() string

	// invalidate clears the kernel cache for this node.
	invalidate(*fs.Server)
}

// DevInoPair uniquely identifies a file outside the sandboxfs.
type DevInoPair struct {
	Device uint64
	Inode  uint64
}

// fileInfoToID retrieves a Device/Inode number pair from info.
func fileInfoToID(info os.FileInfo) DevInoPair {
	stat := info.Sys().(*syscall.Stat_t)
	return DevInoPair{
		Device: uint64(stat.Dev), // Cast required on some platforms (e.g. macOS).
		Inode:  uint64(stat.Ino),
	}
}

// newBaseNode initializes a new BaseNode with a new inode number.
func newBaseNode(path string, id DevInoPair, writable bool) BaseNode {
	return BaseNode{
		inode:          nextInodeNumber(),
		underlyingPath: path,
		underlyingID:   id,
		writable:       writable,
	}
}

// Attr populates 'a' with the file/directory metadata.
func (n *BaseNode) Attr(_ context.Context, a *fuse.Attr) error {
	info, err := os.Lstat(n.underlyingPath)
	if err != nil {
		return fuseErrno(err)
	}
	a.Inode = n.inode
	fillAttrInfo(a, info)
	return nil
}

// Setattr updates the file metadata. While this is also used by the kernel to communicate file size
// changes, there is no concept of a size in the base node. As a result, the caller is responsible
// for handling the size.
//
// This function returns two values: the first is a boolean indicating whether the caller should
// continue trying to apply any attribute changes, and the second carries the first error
// encountered when doing such changes. (A consequence is that if the first value is false, then
// the error must be set, but if the first value is true, the error may not be set.)
//
// Given how Setattr is used to apply one or more attribute changes to a node, it is impossible to
// report all encountered errors back to the kernel. As a result, we just capture the first error
// and return that, ignoring the rest. The caller should do the same when the boolean return value
// is true.
func (n *BaseNode) Setattr(_ context.Context, req *fuse.SetattrRequest) (bool, error) {
	if err := n.WantToWrite(); err != nil {
		return false, err
	}

	var finalError error
	setError := func(err error) {
		if finalError == nil {
			finalError = err
		}
	}

	if req.Valid.Mode() {
		if err := os.Chmod(n.underlyingPath, req.Mode&os.ModePerm); err != nil {
			setError(err)
		}
	}

	if req.Valid.Uid() || req.Valid.Gid() {
		if !(req.Valid.Uid() && req.Valid.Gid()) {
			panic("don't know how to handle setting only Uid or Gid")
		}
		if err := os.Lchown(n.underlyingPath, int(req.Uid), int(req.Gid)); err != nil {
			setError(err)
		}
	}

	if req.Valid.Atime() || req.Valid.Mtime() {
		if !(req.Valid.Atime() && req.Valid.Mtime()) {
			panic("don't know how to handle setting only atime or mtime")
		}
		if err := os.Chtimes(n.underlyingPath, req.Atime, req.Mtime); err != nil {
			setError(err)
		}
	}

	return true, fuseErrno(finalError)
}

// UnderlyingID returns the node's {deviceID, inodeNum} in the underlying filesystem.
func (n *BaseNode) UnderlyingID() DevInoPair {
	return n.underlyingID
}

func newNodeForFileInfo(fileInfo os.FileInfo, path string, id DevInoPair, writable bool) Node {
	switch fileInfo.Mode() & os.ModeType {
	case os.ModeDir:
		return newMappedDir(path, id, writable)
	case os.ModeSymlink:
		return newMappedSymlink(path, id, writable)
	default:
		return newMappedFile(path, id, writable)
	}
}

// Inode returns the node's inode number.
func (n *BaseNode) Inode() uint64 {
	return n.inode
}

// WantToWrite returns nil if the node is writable or the error to report back to the kernel
// otherwise. All operations on nodes that want to modify the state of the file system should call
// this function to ensure all error conditions are consistent.
func (n *BaseNode) WantToWrite() error {
	if !n.writable {
		return fuseErrno(syscall.EPERM)
	}
	return nil
}

// SetUnderlyingPath changes the underlying path value to passed path.
//
// This assumes that the caller takes appropriate steps to prevent concurrency
// issues (by locking the container directory).
func (n *BaseNode) SetUnderlyingPath(path string) {
	n.underlyingPath = path
}

// UnderlyingPath returns the Node's path in the underlying filesystem.
func (n *BaseNode) UnderlyingPath() string {
	return n.underlyingPath
}

// fillAttrInfo manually copies the data from one structure to another.
func fillAttrInfo(a *fuse.Attr, f os.FileInfo) {
	if f.Size() < 0 {
		panic(fmt.Sprintf("Size derived from filesystem was negative: %v", f.Size()))
	}
	a.Size = uint64(f.Size()) // int64 -> uint64

	a.Mode = f.Mode()

	a.Mtime = f.ModTime()
	s := f.Sys().(*syscall.Stat_t)
	a.Atime = Atime(s)
	a.Ctime = Ctime(s)
	a.Uid = s.Uid
	a.Gid = s.Gid

	if f.IsDir() {
		// Directories need to have their link count explicitly set to 2 (and no more than 2
		// because we don't support hard links on directories) to represent the "." and ".."
		// names. FUSE handles those two names internally which means that we never get
		// called back to handle their "creation".
		a.Nlink = 2
	} else {
		a.Nlink = 1
	}

	// Casting before comparison is necessary below because the type of some stat
	// fields have different types on different platforms.
	// For example: Rdev is uint16 on Darwin and MaxUint32 is a constant (untyped).
	// So, before comparison, Go tries to convert MaxUint32 to an uint16 and, since
	// that is not possible, compilation fails.
	//
	// Because uint64 has the maximum range of positive integers, casting to uint64
	// before comparison is safe in all cases.

	if uint64(s.Rdev) > math.MaxUint32 { // See comment above for cast details.
		panic(fmt.Sprintf("Rdev derived from filesystem was larger than MaxUint32: %v", s.Rdev))
	}
	a.Rdev = uint32(s.Rdev) // uint64 -> uint32

	if uint64(s.Blksize) > math.MaxUint32 || s.Blksize < 0 { // See comment above for cast details.
		// Blksize indicates the optimal block size for I/Os on this file.
		// It is OK to ignore this preference if it's out of bounds.
		a.BlockSize = math.MaxUint32
	} else {
		a.BlockSize = uint32(s.Blksize) // int64 -> uint32
	}

	if s.Blocks < 0 {
		panic(fmt.Sprintf("Blocks value derived from filesystem was negative: %v", s.Blocks))
	}
	a.Blocks = uint64(s.Blocks) // int64 -> uint64
}
