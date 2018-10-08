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
use fuse;
use nix::errno::Errno;
use std::ffi::OsStr;
use std::io;
use std::path::PathBuf;
use std::result::Result;
use std::sync::Arc;

mod conv;
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

/// Generic result type for of all node operations.
pub type NodeResult<T> = Result<T, KernelError>;

/// Abstract representation of an open file handle.
pub trait Handle {
    /// Reads `size` bytes from the open file starting at `offset`.
    fn read(&self, offset: i64, size: u32) -> NodeResult<Vec<u8>>;
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

    /// Reads the target of a symlink.
    fn readlink(&self) -> NodeResult<PathBuf> {
        panic!("Not implemented");
    }
}
