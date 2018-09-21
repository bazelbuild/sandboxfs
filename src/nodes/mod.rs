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

use fuse;
use libc;
use std::ffi::OsStr;
use std::fs;
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
    errno: i32,
}

impl KernelError {
    /// Constructs a new error given a raw errno code.
    fn from_errno(errno: i32) -> KernelError {
        KernelError { errno }
    }

    /// Obtains the errno code contained in this error, which can be fed back into the kernel.
    pub fn errno(&self) -> i32 {
        self.errno
    }
}

impl From<io::Error> for KernelError {
    fn from(e: io::Error) -> Self {
        match e.raw_os_error() {
            Some(errno) => KernelError::from_errno(errno),
            None => {
                warn!("Got io::Error without an errno; propagating as EIO: {}", e);
                KernelError::from_errno(libc::EIO)
            },
        }
    }
}

/// Generic result type for of all node operations.
pub type NodeResult<T> = Result<T, KernelError>;

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
    fn lookup(&self, _name: &OsStr) -> NodeResult<(Arc<Node>, fuse::FileAttr)> {
        panic!("Not implemented");
    }

    /// Reads all directory entries into the given reply object.
    ///
    /// It is the responsibility of the caller to invoke `reply.ok()` on the reply object.  This is
    /// for consistency with the handling of any errors returned by this function.
    fn readdir(&self, _reply: &mut fuse::ReplyDirectory) -> NodeResult<()> {
        panic!("Not implemented");
    }
}

/// Computes the new attributes for a node.
///
/// This is a helper function for the implementation of `getattr` and, as such, its signature is
/// tightly coupled to the needs of that function.
///
/// `inode` is the inode number that the node has within sandboxfs.  `path` is the underlying path
/// that the node is mapped to, or none if it's not mapped.  `check_type` is a lambda that validates
/// the file type we obtain from a stat operation and returns the user-facing name of the node type
/// as an error on a mismatch.
///
/// Returns the new attributes if the ones in the node have to be updated, or none if there are no
/// attributes to update.
pub fn get_new_attr<T>(inode: u64, path: Option<&PathBuf>, check_type: T)
    -> NodeResult<Option<fuse::FileAttr>>
    where T: Fn(fs::FileType) -> Result<(), &'static str>
{
    match path {
        Some(path) => {
            let fs_attr = fs::symlink_metadata(path)?;
            if let Err(type_name) = check_type(fs_attr.file_type()) {
                warn!("Path {:?} backing a {} node is no longer a {}; got {:?}",
                        path, type_name, type_name, fs_attr.file_type());
                return Err(KernelError::from_errno(libc::EIO));
            }
            Ok(Some(conv::attr_fs_to_fuse(path, inode, &fs_attr)))
        },
        None => Ok(None),
    }
}
