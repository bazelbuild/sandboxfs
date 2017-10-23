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
	"sync"
	"syscall"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

const (
	// attrValidTime is the default node validity period.
	//
	// TODO(jmmv): This should probably be customizable but, for now, just use the same default
	// as the FUSE library uses when Getattr is not implemented.
	attrValidTime = 1 * time.Minute
)

// BaseNode is a common type for all nodes: files, directories, pipes, symlinks, etc.
type BaseNode struct {
	// underlyingPath contains the path on the file system that backs this node.
	underlyingPath string

	// underlyingID is a unique identifier for the underlying file that backs this node.
	underlyingID DevInoPair

	// writable indicates whether this is a node for a read-only mapping or for a read/write
	// one.
	//
	// TODO(jmmv): It'd be nice if this property was represented by different nodes
	// (e.g. separate ReadOnlyMappedDir and ReadWriteMappedDir) so that, after instantiation,
	// the node wouldn't need to keep checking if it's writable or not.
	writable bool

	// mu protects accesses and updates to the node's metadata below.
	mu sync.Mutex

	// attr contains the node metadata.
	//
	// The node metadata is first populated with details from the underlying file system but is
	// then kept up-to-date in-memory with any updates that happen to the file
	// system. Maintaining this data in memory is necessary so that Setattr can apply partial
	// updates to node ownerships and times.
	//
	// It is possible to access the inode number (through the Inode member function) without
	// holding "mu" because the inode number is immutable throughout the lifetime of the node.
	attr fuse.Attr

	// deleted tracks whether the node has been deleted or not.
	//
	// We need to track this explicitly because nodes can be alive when open handles exist for
	// them even when all the directory entries pointing at them are unlinked. In that case,
	// further node updates must happen in-memory only and cannot be propagated to the
	// underlying file system -- because the entry there is gone.
	//
	// TODO(jmmv): This is insufficient to implement fully-correct semantics. Consider the case
	// of a file with two hard links in the underlying file system, one mapped through sandboxfs
	// and one not. If the file within the sandbox was opened, then deleted, and then updated
	// via any of the f* system calls, the updates should be reflected on disk because the file
	// is still reachable. Fixing this would involve tracking open handles for every node, which
	// adds some overhead and needs to be benchmarked before implementing it.
	//
	// TODO(jmmv): It *should* be possible to track this using attr.Nlink==0, but: first, an
	// attempt at doing so didn't work; and, second, macOS doesn't seem to report Nlink as 0
	// even for real file systems... so maybe this is not true.
	deleted bool
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

	// delete tells the node that a directory entry pointing to it has been removed.
	delete()

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
func newBaseNode(path string, fileInfo os.FileInfo, writable bool) BaseNode {
	var attr fuse.Attr
	attr.Inode = nextInodeNumber()
	attr.Nlink = 1
	attr.Valid = attrValidTime
	fillAttrInfoFromStat(&attr, fileInfo)

	return BaseNode{
		underlyingPath: path,
		underlyingID:   fileInfoToID(fileInfo),
		writable:       writable,
		deleted:        false,
		attr:           attr,
	}
}

// Attr populates 'a' with the file/directory metadata.
func (n *BaseNode) Attr(_ context.Context, a *fuse.Attr) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if !n.deleted {
		fileInfo, err := os.Lstat(n.underlyingPath)
		if err != nil {
			return fuseErrno(err)
		}
		fillAttrInfoFromStat(&n.attr, fileInfo)
	}
	*a = n.attr
	return nil
}

// delete tells the node that a directory entry pointing to it has been removed.
func (n *BaseNode) delete() {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.deleted {
		panic("Cannot delete a node twice; there currently is no support for hard links so this should not have happened")
	}
	n.deleted = true
}

// setattrMode is a helper function for Setattr to handle setting the node's mode.
// The "mu" lock must be held by the caller.
func (n *BaseNode) setattrMode(req *fuse.SetattrRequest) error {
	if !n.deleted {
		if err := os.Chmod(n.underlyingPath, req.Mode&os.ModePerm); err != nil {
			return err
		}
	}
	n.attr.Mode = req.Mode
	return nil
}

// setattrOwnership is a helper function for Setattr to handle setting the node's UID and GID.
// The "mu" lock must be held by the caller.
func (n *BaseNode) setattrOwnership(req *fuse.SetattrRequest) error {
	var uid uint32
	if req.Valid.Uid() {
		uid = req.Uid
	} else {
		uid = n.attr.Uid
	}

	var gid uint32
	if req.Valid.Gid() {
		gid = req.Gid
	} else {
		gid = n.attr.Gid
	}

	if !n.deleted {
		if err := os.Lchown(n.underlyingPath, int(uid), int(gid)); err != nil {
			return err
		}
	}
	n.attr.Uid = uid
	n.attr.Gid = gid
	return nil
}

// setattrTimes is a helper function for Setattr to handle setting the node's atime and mtime.
// The "mu" lock must be held by the caller.
func (n *BaseNode) setattrTimes(req *fuse.SetattrRequest) error {
	var atime time.Time
	if req.Valid.Atime() {
		atime = req.Atime
	} else {
		atime = n.attr.Atime
	}

	var mtime time.Time
	if req.Valid.Mtime() {
		mtime = req.Mtime
	} else {
		mtime = n.attr.Mtime
	}

	if !n.deleted {
		if err := os.Chtimes(n.underlyingPath, atime, mtime); err != nil {
			return err
		}
	}
	n.attr.Atime = atime
	n.attr.Mtime = mtime
	return nil
}

// setattrSize is a helper function for Setattr to handle setting the node's size.
// The "mu" lock must be held by the caller.
func (n *BaseNode) setattrSize(req *fuse.SetattrRequest) error {
	if !n.deleted {
		if err := os.Truncate(n.underlyingPath, int64(req.Size)); err != nil {
			return err
		}
	}
	n.attr.Size = req.Size
	return nil
}

// Setattr updates the file metadata.
//
// Given how Setattr is used to apply one or more attribute changes to a node, it is impossible to
// report all encountered errors back to the kernel. As a result, we just capture the first error
// and return that, ignoring the rest.
func (n *BaseNode) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	if err := n.WantToWrite(); err != nil {
		return err
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	var finalError error
	if req.Valid.Mode() {
		if err := n.setattrMode(req); err != nil && finalError == nil {
			finalError = err
		}
	}
	if req.Valid.Uid() || req.Valid.Gid() {
		if err := n.setattrOwnership(req); err != nil && finalError == nil {
			finalError = err
		}
	}
	if req.Valid.Atime() || req.Valid.Mtime() {
		if err := n.setattrTimes(req); err != nil && finalError == nil {
			finalError = err
		}
	}
	if req.Valid.Size() {
		if err := n.setattrSize(req); err != nil && finalError == nil {
			finalError = err
		}
	}
	return fuseErrno(finalError)
}

// UnderlyingID returns the node's {deviceID, inodeNum} in the underlying filesystem.
func (n *BaseNode) UnderlyingID() DevInoPair {
	return n.underlyingID
}

// newNodeForFileInfo creates a new node based on the stat information of an underlying file.
func newNodeForFileInfo(path string, fileInfo os.FileInfo, writable bool) Node {
	switch fileInfo.Mode() & os.ModeType {
	case os.ModeDir:
		return newMappedDir(path, fileInfo, writable)
	case os.ModeSymlink:
		return newMappedSymlink(path, fileInfo, writable)
	default:
		return newMappedFile(path, fileInfo, writable)
	}
}

// Inode returns the node's inode number.
func (n *BaseNode) Inode() uint64 {
	// Given that the inode is immutable, it's OK to access this property without holding mu.
	return n.attr.Inode
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

// fillAttrInfoFromStat populates a fuse.Attr instance with the results of a Stat operation.
//
// This does not copy the properties of the node that do not make sense in the sandboxfs context.
// For example: the inode value and the number of links are details that are internally maintained
// by sandboxfs and the underlying values have no meaning here.
func fillAttrInfoFromStat(a *fuse.Attr, f os.FileInfo) {
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
