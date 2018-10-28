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
use failure::Error;
use fuse;
use nix;
use nix::errno::Errno;
use nix::{sys, unistd};
use std::ffi::OsStr;
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

/// Updates the metadata of a file given a delta of attributes.
///
/// This is a helper function to implement `Node::setattr` for the various node types.  The caller
/// is responsible for obtaining the updated metadata of the underlying file to return it to the
/// kernel, as computing the updated metadata here would be inaccurate.  The reason is that we don't
/// track ctimes ourselves so any modifications to the file cause the backing ctime to be updated
/// and we need to obtain it.
/// TODO(https://github.com/bazelbuild/sandboxfs/issues/43): Compute the modified fuse::FileAttr and
/// return it once we track ctimes.
///
/// This tries to apply as many properties as possible in case of errors.  When errors occur,
/// returns the first that was encountered.
pub fn setattr(path: &Path, attr: &fuse::FileAttr, delta: &AttrDelta) -> Result<(), nix::Error> {
    let mut result = Ok(());

    if let Some(mode) = delta.mode {
        result = result.and(
            sys::stat::fchmodat(None, path, mode, sys::stat::FchmodatFlags::NoFollowSymlink));
    }

    if delta.uid.is_some() || delta.gid.is_some() {
        result = result.and(
            unistd::fchownat(None, path, delta.uid, delta.gid,
                unistd::FchownatFlags::NoFollowSymlink));
    }

    if delta.atime.is_some() || delta.mtime.is_some() {
        let atime = delta.atime.unwrap_or_else(|| conv::timespec_to_timeval(attr.atime));
        let mtime = delta.mtime.unwrap_or_else(|| conv::timespec_to_timeval(attr.mtime));
        if attr.kind == fuse::FileType::Symlink {
            // TODO(jmmv): Should use futimensat to avoid following symlinks but this function is
            // not available on macOS El Capitan, which we currently require for CI builds.
            warn!("Asked to update atime/mtime for symlink {} but following symlink instead",
                path.display());
        }
        result = result.and(sys::stat::utimes(path, &atime, &mtime));
    }

    if let Some(size) = delta.size {
        // Updating the size only makes sense on files, but handling it here is much simpler than
        // doing so on a node type basis.  Plus, who knows, if the kernel asked us to change the
        // size of anything other than a file, maybe we have to obey and try to do it.
        if size > ::std::i64::MAX as u64 {
            warn!("truncate request got size {}, which is too large (exceeds i64's MAX)", size);
            result = result.and(Err(nix::Error::invalid_argument()));
        } else {
            result = result.and(unistd::truncate(path, size as i64));
        }
    }

    result
}

/// Abstract representation of an open file handle.
pub trait Handle {
    /// Reads `size` bytes from the open file starting at `offset`.
    fn read(&self, _offset: i64, _size: u32) -> NodeResult<Vec<u8>> {
        panic!("Not implemented")
    }

    /// Reads all directory entries into the given reply object.
    ///
    /// While this takes a `fuse::ReplyDirectory` object as a parameter for efficiency reasons, it
    /// is the responsibility of the caller to invoke `reply.ok()` and `reply.error()` on the same
    /// reply object.  This is for consistency with the handling of any errors returned by this and
    /// other functions.
    ///
    /// `_ids` and `_cache` are the file system-wide bookkeeping objects needed to instantiate new
    /// nodes, used when readdir discovers an underlying node that was not yet known.
    fn readdir(&self, _ids: &IdGenerator, _cache: &Cache, _reply: &mut fuse::ReplyDirectory)
        -> NodeResult<()> {
        panic!("Not implemented");
    }
}

/// Abstract representation of a file system node.
///
/// Due to the way nodes and node operations are represented in the kernel, this trait exposes a
/// collection of methods that do not all make sense for all possible node types: some methods will
/// only make sense for directories and others will only make sense for regular files, for example.
/// These conflicting methods come with a default implementation that panics.
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

    /// Maps a path onto a node and creates intermediate components as immutable directories.
    ///
    /// `_components` is the path to map, broken down into components, and relative to the current
    /// node.  `_underlying_path` is the target to use for the created node.  `_writable` indicates
    /// the final node's writability, but intermediate nodes are creates as not writable.
    ///
    /// `_ids` and `_cache` are the file system-wide bookkeeping objects needed to instantiate new
    /// nodes, used when this algorithm instantiates any new node.
    fn map(&self, _components: &[Component], _underlying_path: &Path, _writable: bool,
        _ids: &IdGenerator, _cache: &Cache) -> Result<(), Error> {
        panic!("Not implemented")
    }

    /// Retrieves the node's metadata.
    fn getattr(&self) -> NodeResult<fuse::FileAttr>;

    /// Looks up a node with the given name within the current node and returns the found node and
    /// its attributes at the time of the query.
    ///
    /// The attributes are returned to avoid having to relock the node on the caller side in order
    /// to supply those attributes to the kernel.
    ///
    /// `_ids` and `_cache` are the file system-wide bookkeeping objects needed to instantiate new
    /// nodes, used when lookup discovers an underlying node that was not yet known.
    fn lookup(&self, _name: &OsStr, _ids: &IdGenerator, _cache: &Cache)
        -> NodeResult<(Arc<Node>, fuse::FileAttr)> {
        panic!("Not implemented");
    }

    /// Opens the file and returns an open file handle for it.
    fn open(&self, _flags: u32) -> NodeResult<Arc<Handle>> {
        panic!("Not implemented");
    }

    /// Reads the target of a symlink.
    fn readlink(&self) -> NodeResult<PathBuf> {
        panic!("Not implemented");
    }

    /// Sets one or more properties of the node's metadata.
    fn setattr(&self, _delta: &AttrDelta) -> NodeResult<fuse::FileAttr>;
}
