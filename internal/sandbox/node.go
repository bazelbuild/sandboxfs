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

var (
	// nodeCacheLock is the mutex that protects all accesses to nodeCache.
	//
	// Given that the cache is accessed on every node creation, the critical sections protected
	// by this lock should be as short as possible. In particular, do not hold this lock before
	// holding each node's lock, as the latter can be held while system calls are in progress.
	// This is an ordering restriction that also helps avoid deadlocks.
	nodeCacheLock sync.Mutex

	// nodeCache holds a cache of sandboxfs nodes indexed by their underlying path.
	//
	// This cache is critical to offer good performance during reconfigurations: if the identity
	// of an underlying file changes across reconfigurations, the kernel will think it's a
	// different file (even if it may not be) and will therefore not be able to take advantage
	// of any caches. You would think that avoiding kernel cache invalidations during the
	// reconfiguration itself (e.g. if file "A" was mapped and is still mapped now, don't
	// invalidate it) would be sufficient to avoid this problem, but it's not: "A" could be
	// mapped, then unmapped, and then remapped again in three different reconfigurations, and
	// we'd still not want to lose track of it.
	//
	// The cache is a property specific to each sandboxfs instance and, as such, it should live
	// within the FS object that represents a mount point. However, doing so would require each
	// node to hold a pointer to the FS, and as we only support a single instance at once, we
	// can save some overhead by just declaring this as a global.
	//
	// Nodes should be inserted in this map at creation time and removed from it when explicitly
	// deleted by the user (because there is a chance they'll be recreated, and at that point we
	// truly want to reload the data from disk).
	//
	// TODO(jmmv): There currently is no cache expiration, which means that memory usage can
	// grow unboundedly. A preliminary attempt at expiring cache entries on a node's Forget
	// handler sounded promising (because then cache expiration would be delegated to the
	// kernel)... but, on Linux, the kernel seems to be calling this very eagerly, rendering our
	// cache useless. I did not track down what exactly triggered the Forget notifications
	// though.
	nodeCache = make(map[string]Node)
)

// BaseNode is a common type for all nodes: files, directories, pipes, symlinks, etc.
type BaseNode struct {
	// writable indicates whether this is a node for a read-only mapping or for a read/write
	// one.
	writable bool

	// mu protects accesses and updates to the node's metadata below.
	mu sync.Mutex

	// optionalUnderlyingPath contains the path on the file system that backs this node.
	//
	// This is empty if the node has no backing file system path (such as for intermediate
	// directories).
	//
	// Always use UnderlyingPath() to query this safely (which explains the convoluted name of
	// this field).
	optionalUnderlyingPath string

	// isMapping indicates whether this node exists because it was an explicit mapping from the
	// configuration.
	isMapping bool

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

	// Dirent returns the directory entry for the node on which it is
	// called.
	// The node's name is the basename of the directory entry (no path components),
	// and needs to be passed in because it is not stored within the node itself.
	Dirent(name string) fuse.Dirent

	// IsMapping returns true if the node was explicitly mapped by the user to a physical
	// location on disk (i.e. if there is an underlying path for the node).
	IsMapping() bool

	// SetIsMapping marks this node as being the mapped path of a mapping explicitly configured
	// by the user.
	SetIsMapping()

	// UnderlyingPath returns the path to the file that backs this node in the underlying file
	// system and whether this path is valid. (The returned string should not be accessed unless
	// the returned boolean is true.)
	UnderlyingPath() (string, bool)

	// SetUnderlyingPath changes the node's underlying path to the specified value. This mutable
	// operation is necessary to support renames.
	SetUnderlyingPath(path string)

	// Writable returns the Node's writability property.
	Writable() bool

	// delete tells the node that a directory entry pointing to it has been removed.
	delete()

	// invalidate clears the kernel cache for this node.
	invalidate(*fs.Server)
}

// getOrCreateNode gets a mapped node from the cache or creates a new one if not yet cached.
//
// The returned node represents the given underlying path uniquely. If creation is needed, the
// created node uses the given type and writable settings.
func getOrCreateNode(path string, fileInfo os.FileInfo, writable bool) Node {
	if fileInfo.Mode()&os.ModeType == os.ModeDir {
		// Directories cannot be cached because they contain entries that are created only
		// in memory based on the mappings configuration.
		return newDir(path, fileInfo, writable)
	}

	nodeCacheLock.Lock()
	defer nodeCacheLock.Unlock()

	if node, ok := nodeCache[path]; ok {
		if writable == node.Writable() {
			// We have a match from the cache! Return it immediately.
			//
			// It is tempting to ensure that the type of the cached node matches the
			// type we want to return based on the fileInfo we have now... but doing so
			// does not really prevent problems: the type of the underlying file can
			// change at any point in time. We could check this here and the type could
			// change immediately afterwards behind our backs.
			return node
		}

		// We had a match... but node writability has changed so recreate the node.
		//
		// You may wonder why we care about this and not the file type as described above:
		// the reason is that the writability property is a setting of the current sandbox
		// configuration, not a property of the underlying files, and thus it's a setting
		// that we fully control and must keep correct.
		log.Printf("Missed node caching opportunity because writability has changed for %s", path)
	}

	var node Node
	switch fileInfo.Mode() & os.ModeType {
	case os.ModeDir:
		panic("Directory entries cannot be cached and are handled above")
	case os.ModeSymlink:
		node = newSymlink(path, fileInfo, writable)
	default:
		node = newFile(path, fileInfo, writable)
	}
	nodeCache[path] = node
	return node
}

// newBaseNode initializes a new BaseNode to represent an underlying path.
//
// The returned BaseNode is not usable by itself: it must be embedded within a specific Mapped*
// type. In turn, this function should only be called by the corresponding newMapped* functions
// and never directly.
func newBaseNode(path string, fileInfo os.FileInfo, writable bool) BaseNode {
	var attr fuse.Attr
	attr.Inode = nextInodeNumber()
	attr.Nlink = 1
	attr.Valid = attrValidTime
	fillAttrInfoFromStat(&attr, fileInfo)

	return BaseNode{
		optionalUnderlyingPath: path,
		writable:               writable,
		deleted:                false,
		attr:                   attr,
	}
}

// newUnmappedBaseNode initializes a new BaseNode that is not backed by an underlying path.
//
// The returned BaseNode is not usable by itself: it must be embedded within a specific Mapped*
// type. In turn, this function should only be called by the corresponding newMapped* functions
// and never directly.
func newUnmappedBaseNode(mode os.FileMode, nlink uint32) BaseNode {
	var attr fuse.Attr
	attr.Inode = nextInodeNumber()
	attr.Mode = mode
	attr.Nlink = nlink
	attr.Valid = attrValidTime

	return BaseNode{
		optionalUnderlyingPath: "",
		writable:               false,
		deleted:                false,
		attr:                   attr,
	}
}

// Attr populates 'a' with the file/directory metadata.
func (n *BaseNode) Attr(_ context.Context, a *fuse.Attr) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if underlyingPath, isMapped := n.UnderlyingPath(); isMapped && !n.deleted {
		fileInfo, err := os.Lstat(underlyingPath)
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

	if underlyingPath, isMapped := n.UnderlyingPath(); isMapped {
		nodeCacheLock.Lock()
		defer nodeCacheLock.Unlock()
		delete(nodeCache, underlyingPath)
	}
}

// setattrMode is a helper function for Setattr to handle setting the node's mode.
// The "mu" lock must be held by the caller.
func (n *BaseNode) setattrMode(req *fuse.SetattrRequest) error {
	if underlyingPath, isMapped := n.UnderlyingPath(); isMapped && !n.deleted {
		if err := os.Chmod(underlyingPath, req.Mode&os.ModePerm); err != nil {
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

	if underlyingPath, isMapped := n.UnderlyingPath(); isMapped && !n.deleted {
		if err := os.Lchown(underlyingPath, int(uid), int(gid)); err != nil {
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

	if underlyingPath, isMapped := n.UnderlyingPath(); isMapped && !n.deleted {
		if err := os.Chtimes(underlyingPath, atime, mtime); err != nil {
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
	if underlyingPath, isMapped := n.UnderlyingPath(); isMapped && !n.deleted {
		if err := os.Truncate(underlyingPath, int64(req.Size)); err != nil {
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

// Inode returns the node's inode number.
func (n *BaseNode) Inode() uint64 {
	// Given that the inode is immutable, it's OK to access this property without holding mu.
	return n.attr.Inode
}

// WantToWrite returns nil if the node is writable or the error to report back to the kernel
// otherwise. All operations on nodes that want to modify the state of the file system should call
// this function to ensure all error conditions are consistent.
func (n *BaseNode) WantToWrite() error {
	if !n.writable { // No need to lock read; writable is immutable.
		return fuseErrno(syscall.EPERM)
	}
	return nil
}

// IsMapping returns true if the node was explicitly mapped by the user to a physical location on
// disk (i.e. if there is an underlying path for the node).
func (n *BaseNode) IsMapping() bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.isMapping
}

// SetIsMapping marks this node as being the mapped path of a mapping explicitly configured by the
// user.
func (n *BaseNode) SetIsMapping() {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.isMapping = true
}

// SetUnderlyingPath changes the underlying path value to passed path.
//
// issues (by locking the container directory).
func (n *BaseNode) SetUnderlyingPath(path string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.optionalUnderlyingPath = path
}

// UnderlyingPath returns the path to the file that backs this node in the underlying file system
// and whether this path is valid. (The returned string should not be accessed unless the returned
// boolean is true.)
func (n *BaseNode) UnderlyingPath() (string, bool) {
	// No need to lock; optionalUnderlyingPath is immutable.
	return n.optionalUnderlyingPath, n.optionalUnderlyingPath != ""
}

// Writable returns the Node's writability property.
func (n *BaseNode) Writable() bool {
	// No need to lock; writable is immutable.
	return n.writable
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
