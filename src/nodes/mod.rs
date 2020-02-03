// Copyright 2018 Google Inc.
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

use {Cache, IdGenerator};
use failure::Fallible;
use fuse;
use nix;
use nix::errno::Errno;
use nix::{sys, unistd};
use std::ffi::OsStr;
use std::fs;
use std::io;
use std::path::{Component, Path, PathBuf};
use std::result::Result;
use std::sync::Arc;

pub mod conv;
mod dir;
pub use self::dir::Dir;
mod file;
pub use self::file::File;
mod symlink;
pub use self::symlink::Symlink;

/// Type that represents an error understood by the kernel.
#[derive(Debug, Fail)]
#[fail(display = "errno={}", errno)]
pub struct KernelError {
    errno: Errno,
}

impl KernelError {
    /// Constructs a new error given a raw errno code.
    fn from_errno(errno: Errno) -> KernelError {
        KernelError { errno }
    }

    /// Obtains the errno code contained in this error as an integer.
    pub fn errno_as_i32(&self) -> i32 {
        self.errno as i32
    }
}

impl From<io::Error> for KernelError {
    fn from(e: io::Error) -> Self {
        match e.raw_os_error() {
            Some(errno) => KernelError::from_errno(Errno::from_i32(errno)),
            None => {
                warn!("Got io::Error without an errno; propagating as EIO: {}", e);
                KernelError::from_errno(Errno::EIO)
            },
        }
    }
}

impl From<nix::Error> for KernelError {
    fn from(e: nix::Error) -> Self {
        match e {
            nix::Error::Sys(errno) => KernelError::from_errno(errno),
            _ => {
                warn!("Got nix::Error without an errno; propagating as EIO: {}", e);
                KernelError::from_errno(Errno::EIO)
            }
        }
    }
}

/// Container for new attribute values to set on any kind of node.
pub struct AttrDelta {
    pub mode: Option<sys::stat::Mode>,
    pub uid: Option<unistd::Uid>,
    pub gid: Option<unistd::Gid>,
    pub atime: Option<sys::time::TimeVal>,
    pub mtime: Option<sys::time::TimeVal>,
    pub size: Option<u64>,
}

/// Generic result type for of all node operations.
pub type NodeResult<T> = Result<T, KernelError>;

/// Applies an operation to a path if present, or otherwise returns Ok.
fn try_path<O: Fn(&PathBuf) -> nix::Result<()>>(path: Option<&PathBuf>, op: O) -> nix::Result<()> {
    match path {
        Some(path) => op(path),
        None => Ok(()),
    }
}

/// Helper function for `setattr` to apply only the mode changes.
fn setattr_mode(attr: &mut fuse::FileAttr, path: Option<&PathBuf>, mode: Option<sys::stat::Mode>)
    -> Result<(), nix::Error> {
    if mode.is_none() {
        return Ok(())
    }
    let mode = mode.unwrap();

    if mode.bits() > sys::stat::mode_t::from(std::u16::MAX) {
        warn!("Got setattr with mode {:?} for {:?} (inode {}), which is too large; ignoring",
            mode, path, attr.ino);
        return Err(nix::Error::from_errno(Errno::EIO));
    }
    let perm = mode.bits() as u16;

    if attr.kind == fuse::FileType::Symlink {
        // TODO(jmmv): Should use NoFollowSymlink to support changing the mode of a symlink if
        // requested to do so, but this is not supported on Linux.
        return Err(nix::Error::from_errno(Errno::EOPNOTSUPP));
    }

    let result = try_path(path, |p|
        sys::stat::fchmodat(None, p, mode, sys::stat::FchmodatFlags::FollowSymlink));
    if result.is_ok() {
        attr.perm = perm;
    }
    result
}

/// Helper function for `setattr` to apply only the UID and GID changes.
fn setattr_owners(attr: &mut fuse::FileAttr, path: Option<&PathBuf>, uid: Option<unistd::Uid>,
    gid: Option<unistd::Gid>) -> Result<(), nix::Error> {
    if uid.is_none() && gid.is_none() {
        return Ok(())
    }

    let result = try_path(path, |p|
        unistd::fchownat(None, p, uid, gid, unistd::FchownatFlags::NoFollowSymlink));
    if result.is_ok() {
        attr.uid = uid.map_or(attr.uid, u32::from);
        attr.gid = gid.map_or(attr.gid, u32::from);
    }
    result
}

/// Helper function for `setattr` to apply only the atime and mtime changes.
fn setattr_times(attr: &mut fuse::FileAttr, path: Option<&PathBuf>,
    atime: Option<sys::time::TimeVal>, mtime: Option<sys::time::TimeVal>)
    -> Result<(), nix::Error> {
    if atime.is_none() && mtime.is_none() {
        return Ok(());
    }

    if attr.kind == fuse::FileType::Symlink {
        // TODO(https://github.com/bazelbuild/sandboxfs/issues/46): Should use futimensat to support
        // changing the times of a symlink if requested to do so.
        return Err(nix::Error::from_errno(Errno::EOPNOTSUPP));
    }

    let atime = atime.unwrap_or_else(|| conv::timespec_to_timeval(attr.atime));
    let mtime = mtime.unwrap_or_else(|| conv::timespec_to_timeval(attr.mtime));
    let result = try_path(path, |p| sys::stat::utimes(p, &atime, &mtime));
    if result.is_ok() {
        attr.atime = conv::timeval_to_timespec(atime);
        attr.mtime = conv::timeval_to_timespec(mtime);
    }
    result
}

/// Helper function for `setattr` to apply only the size changes.
fn setattr_size(attr: &mut fuse::FileAttr, path: Option<&PathBuf>, size: Option<u64>)
    -> Result<(), nix::Error> {
    if size.is_none() {
        return Ok(());
    }
    let size = size.unwrap();

    let result = if size > ::std::i64::MAX as u64 {
        warn!("truncate request got size {}, which is too large (exceeds i64's MAX)", size);
        Err(nix::Error::invalid_argument())
    } else {
        try_path(path, |p| unistd::truncate(p, size as i64))
    };
    if result.is_ok() {
        attr.size = size;
    }
    result
}

/// Updates the metadata of a file given a delta of attributes.
///
/// This is a helper function to implement `Node::setattr` for the various node types.
///
/// This tries to apply as many properties as possible in case of errors.  When errors occur,
/// returns the first that was encountered.
pub fn setattr(path: Option<&PathBuf>, attr: &fuse::FileAttr, delta: &AttrDelta)
    -> Result<fuse::FileAttr, nix::Error> {
    // Compute the potential new ctime for these updates.  We want to avoid picking a ctime that is
    // larger than what the operations below can result in (so as to prevent a future getattr from
    // moving the ctime back) which is tricky because we don't know the time resolution of the
    // underlying file system.  Some, like HFS+, only have 1-second resolution... so pick that in
    // the worst case.
    //
    // This is not perfectly accurate because we don't actually know what ctime the underlying file
    // has gotten and thus a future getattr on it will cause the ctime to change.  But as long as we
    // avoid going back on time, we can afford to do this -- which is a requirement for supporting
    // setting attributes on deleted files.
    //
    // TODO(https://github.com/bazelbuild/sandboxfs/issues/43): Revisit this when we track
    // ctimes purely on our own.
    let updated_ctime = {
        let mut now = time::now().to_timespec();
        now.nsec = 0;
        if attr.ctime > now {
            attr.ctime
        } else {
            now
        }
    };

    let mut new_attr = *attr;
    // Intentionally using and() instead of and_then() to try to apply all changes but to only
    // keep the first error result.
    let result = Ok(())
        .and(setattr_mode(&mut new_attr, path, delta.mode))
        .and(setattr_owners(&mut new_attr, path, delta.uid, delta.gid))
        .and(setattr_times(&mut new_attr, path, delta.atime, delta.mtime))
        // Updating the size only makes sense on files, but handling it here is much simpler than
        // doing so on a node type basis.  Plus, who knows, if the kernel asked us to change the
        // size of anything other than a file, maybe we have to obey and try to do it.
        .and(setattr_size(&mut new_attr, path, delta.size));
    if !conv::fileattrs_eq(attr, &new_attr) {
        new_attr.ctime = updated_ctime;
    }
    result.and(Ok(new_attr))
}

/// Abstract representation of an open file handle.
pub trait Handle {
    /// Reads `_size` bytes from the open file starting at `_offset`.
    fn read(&self, _offset: i64, _size: u32) -> NodeResult<Vec<u8>> {
        panic!("Not implemented")
    }

    /// Reads directory entries into the given reply object until it is full.
    ///
    /// `_offset` indicates the "identifier" of the *last* entry we returned to the kernel in a past
    /// `readdir` call, not the index of the first entry to return.  This difference is subtle but
    /// important, as an offset of zero has to be handled especially.
    ///
    /// While this takes a `fuse::ReplyDirectory` object as a parameter for efficiency reasons, it
    /// is the responsibility of the caller to invoke `reply.ok()` and `reply.error()` on the same
    /// reply object.  This is for consistency with the handling of any errors returned by this and
    /// other functions.
    ///
    /// `_ids` and `_cache` are the file system-wide bookkeeping objects needed to instantiate new
    /// nodes, used when readdir discovers an underlying node that was not yet known.
    fn readdir(&self, _ids: &IdGenerator, _cache: &Cache, _offset: i64,
        _reply: &mut fuse::ReplyDirectory) -> NodeResult<()> {
        panic!("Not implemented");
    }

    /// Writes the bytes held in `_data` to the open file starting at `_offset`.
    fn write(&self, _offset: i64, _data: &[u8]) -> NodeResult<u32> {
        panic!("Not implemented");
    }
}

/// A reference-counted `Handle` that's safe to send across threads.
pub type ArcHandle = Arc<dyn Handle + Send + Sync>;

/// Abstract representation of a file system node.
///
/// Due to the way nodes and node operations are represented in the kernel, this trait exposes a
/// collection of methods that do not all make sense for all possible node types: some methods will
/// only make sense for directories and others will only make sense for regular files, for example.
/// These conflicting methods come with a default implementation that panics.
//
// TODO(jmmv): We should split the Node into three traits: one for methods that are not vnops,
// one for read-only vnops, and one for read/write vnops.  While our global tracking data structures
// will not be able to differentiate between the three, call sites will (e.g. when retrieving nodes
// for write, which could make the code simpler and safer).  And if going this route, consider how
// we could also handle vnop "classes" with traits (so that, e.g. a File wouldn't need to have
// default implementations for the Dir vnops).
pub trait Node {
    /// Returns the inode number of this node.
    ///
    /// The inode number is immutable and, as such, this information can be queried without having
    /// to lock the node.
    fn inode(&self) -> u64;

    /// Returns whether the node is writable or not.
    ///
    /// The node's writability is immutable and, as such, this information can be queried without
    /// having to lock the node.
    fn writable(&self) -> bool;

    /// Retrieves the node's file type without refreshing it from disk.
    ///
    /// Knowing the file type is necessary only in the very specific case of returning explicitly
    /// mapped directory entries as part of readdir.  We can tolerate not refreshing this
    /// information as part of the readdir, which would be costly.  The worst that can happen is
    /// that, if the type of an underlying path changes, readdir wouldn't notice until the cached
    /// `getattr` data becomes stale; given that this will always be a problem for `Dir`s and
    /// `Symlink`s (on which we don't allow type changes at all), it's OK.
    fn file_type_cached(&self) -> fuse::FileType;

    /// Marks the node as deleted (though the in-memory representation remains).
    ///
    /// The implication of this is that a node loses its backing underlying path once this method
    /// is called.
    fn delete(&self);

    /// Updates the node's underlying path to the given one.  Needed for renames.
    fn set_underlying_path(&self, _path: &Path);

    /// Maps a path onto a node and creates intermediate components as immutable directories.
    ///
    /// `_components` is the path to map, broken down into components, and relative to the current
    /// node.  `_underlying_path` is the target to use for the created node.  `_writable` indicates
    /// the final node's writability, but intermediate nodes are creates as not writable.
    ///
    /// `_ids` and `_cache` are the file system-wide bookkeeping objects needed to instantiate new
    /// nodes, used when this algorithm instantiates any new node.
    fn map(&self, _components: &[Component], _underlying_path: &Path, _writable: bool,
        _ids: &IdGenerator, _cache: &Cache) -> Fallible<()> {
        panic!("Not implemented")
    }

    /// Unmaps a path.
    ///
    /// `_components` is the path to map, broken down into components, and relative to the current
    /// node.
    fn unmap(&self, _components: &[Component]) -> Fallible<()> {
        panic!("Not implemented")
    }

    /// Creates a new file with `_name` and `_mode` and opens it with `_flags`.
    ///
    /// The attributes are returned to avoid having to relock the node on the caller side in order
    /// to supply those attributes to the kernel.
    ///
    /// `_ids` and `_cache` are the file system-wide bookkeeping objects needed to instantiate new
    /// nodes, used when create has to instantiate a new node.
    #[allow(clippy::too_many_arguments, clippy::type_complexity)]
    fn create(&self, _name: &OsStr, _uid: unistd::Uid, _gid: unistd::Gid, _mode: u32, _flags: u32,
        _ids: &IdGenerator, _cache: &Cache) -> NodeResult<(ArcNode, ArcHandle, fuse::FileAttr)> {
        panic!("Not implemented")
    }

    /// Retrieves the node's metadata.
    fn getattr(&self) -> NodeResult<fuse::FileAttr>;

    /// Gets the value of the `_name` exgtended attribute.
    fn getxattr(&self, _name: &OsStr) -> NodeResult<Option<Vec<u8>>> {
        panic!("Not implemented");
    }

    /// Creates a handle for an already-open backing file corresponding to this node.
    fn handle_from(&self, _file: fs::File) -> ArcHandle {
        panic!("Not implemented");
    }

    /// Gets the list of extended attribute names from the file.
    fn listxattr(&self) -> NodeResult<xattr::XAttrs> {
        panic!("Not implemented");
    }

    /// Looks up a node with the given name within the current node and returns the found node and
    /// its attributes at the time of the query.
    ///
    /// The attributes are returned to avoid having to relock the node on the caller side in order
    /// to supply those attributes to the kernel.
    ///
    /// `_ids` and `_cache` are the file system-wide bookkeeping objects needed to instantiate new
    /// nodes, used when lookup discovers an underlying node that was not yet known.
    fn lookup(&self, _name: &OsStr, _ids: &IdGenerator, _cache: &Cache)
        -> NodeResult<(ArcNode, fuse::FileAttr)> {
        panic!("Not implemented");
    }

    /// Creates a new directory with `_name` and `_mode`.
    ///
    /// The attributes are returned to avoid having to relock the node on the caller side in order
    /// to supply those attributes to the kernel.
    ///
    /// `_ids` and `_cache` are the file system-wide bookkeeping objects needed to instantiate new
    /// nodes, used when create has to instantiate a new node.
    #[allow(clippy::too_many_arguments)]
    fn mkdir(&self, _name: &OsStr, _uid: unistd::Uid, _gid: unistd::Gid, _mode: u32,
        _ids: &IdGenerator, _cache: &Cache) -> NodeResult<(ArcNode, fuse::FileAttr)> {
        panic!("Not implemented")
    }

    /// Creates a new special file with `_name`, `_mode`, and `_rdev`.
    ///
    /// The attributes are returned to avoid having to relock the node on the caller side in order
    /// to supply those attributes to the kernel.
    ///
    /// `_ids` and `_cache` are the file system-wide bookkeeping objects needed to instantiate new
    /// nodes, used when create has to instantiate a new node.
    #[allow(clippy::too_many_arguments)]
    fn mknod(&self, _name: &OsStr, _uid: unistd::Uid, _gid: unistd::Gid, _mode: u32, _rdev: u32,
        _ids: &IdGenerator, _cache: &Cache) -> NodeResult<(ArcNode, fuse::FileAttr)> {
        panic!("Not implemented")
    }

    /// Opens the file and returns an open file handle for it.
    fn open(&self, _flags: u32) -> NodeResult<ArcHandle> {
        panic!("Not implemented");
    }

    /// Reads the target of a symlink.
    fn readlink(&self) -> NodeResult<PathBuf> {
        panic!("Not implemented");
    }

    /// Removes the `_name` extended attribute.
    fn removexattr(&self, _name: &OsStr) -> NodeResult<()> {
        panic!("Not implemented");
    }

    /// Renames the entry `_name` to `_new_name`.
    ///
    /// This operation is separate from "rename and move" because it acts on a single directory and
    /// therefore we only need to lock one node.
    ///
    /// `_cache` is the file system-wide bookkeeping object that caches underlying paths to nodes,
    /// which needs to be update to account for the node rename.
    fn rename(&self, _name: &OsStr, _new_name: &OsStr, _cache: &Cache) -> NodeResult<()> {
        panic!("Not implemented");
    }

    /// Moves the given `_old_name` to the directory `_new_dir` and renames it to `_new_name`.
    ///
    /// Because of the necessity to lock two nodes during a move, this operation cannot be used when
    /// the source and target are in the same directory.  Doing so would trigger an attempt to
    /// double-lock the node's state.  Use `rename` instead.
    ///
    /// This is the "first half" of the move operation.  This hook locks the source node, extracts
    /// any necessary information from it, and then delegates to the target node to complete the
    /// move.  The source node must remain locked for the duration of the move.
    ///
    /// `_cache` is the file system-wide bookkeeping object that caches underlying paths to nodes,
    /// which needs to be update to account for the node rename.
    fn rename_and_move_source(&self, _old_name: &OsStr, _new_dir: ArcNode, _new_name: &OsStr,
        _cache: &Cache) -> NodeResult<()> {
        panic!("Not implemented");
    }

    /// Attaches the given `_dirent` to this directory and renames it to `_new_name`.
    ///
    /// `_old_path` contains the underlying path of the node being moved in its source directory.
    ///
    /// This is the "second half" of the move operation.  This hook locks the target node and
    /// assumes that the source node is still locked, then attempts to move the underlying file,
    /// attaches the directory entry to the current node, and finally updates its name to the new
    /// name.
    ///
    /// If the target node is not a directory, this operation shall fail with `ENOTDIR` (the
    /// default implementation).
    ///
    /// `_cache` is the file system-wide bookkeeping object that caches underlying paths to nodes,
    /// which needs to be update to account for the node rename.
    fn rename_and_move_target(&self, _dirent: &dir::Dirent, _old_path: &Path, _new_name: &OsStr,
        _cache: &Cache) -> NodeResult<()> {
        Err(KernelError::from_errno(Errno::ENOTDIR))
    }

    /// Deletes the empty directory `_name`.
    ///
    /// `_cache` is the file system-wide bookkeeping object that caches underlying paths to nodes,
    /// which needs to be update to account for the node removal.
    fn rmdir(&self, _name: &OsStr, _cache: &Cache) -> NodeResult<()> {
        panic!("Not implemented");
    }

    /// Sets one or more properties of the node's metadata.
    fn setattr(&self, _delta: &AttrDelta) -> NodeResult<fuse::FileAttr>;

    /// Sets the `_name` extended attribute to `_value`.
    fn setxattr(&self, _name: &OsStr, _value: &[u8]) -> NodeResult<()> {
        panic!("Not implemented");
    }

    /// Creates a symlink with `_name` pointing at `_link`.
    ///
    /// The attributes are returned to avoid having to relock the node on the caller side in order
    /// to supply those attributes to the kernel.
    ///
    /// `_ids` and `_cache` are the file system-wide bookkeeping objects needed to instantiate new
    /// nodes, used when create has to instantiate a new node.
    fn symlink(&self, _name: &OsStr, _link: &Path, _uid: unistd::Uid, _gid: unistd::Gid,
        _ids: &IdGenerator, _cache: &Cache) -> NodeResult<(ArcNode, fuse::FileAttr)> {
        panic!("Not implemented")
    }

    /// Deletes the file `_name`.
    ///
    /// `_cache` is the file system-wide bookkeeping object that caches underlying paths to nodes,
    /// which needs to be update to account for the node removal.
    fn unlink(&self, _name: &OsStr, _cache: &Cache) -> NodeResult<()> {
        panic!("Not implemented");
    }
}

/// A reference-counted `Node` that's safe to send across threads.
pub type ArcNode = Arc<dyn Node + Send + Sync>;
