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

extern crate fuse;
extern crate time;

use nix::{errno, unistd};
use self::time::Timespec;
use std::ffi::OsStr;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use super::conv;
use super::KernelError;
use super::Node;
use super::NodeResult;

/// Representation of a directory node.
pub struct Dir {
    inode: u64,
    writable: bool,
    state: Mutex<MutableDir>,
}

/// Holds the mutable data of a directory node.
struct MutableDir {
    parent: u64,
    underlying_path: Option<PathBuf>,
    attr: fuse::FileAttr,
}

impl Dir {
    /// Creates a new scaffold directory to represent the root directory.
    ///
    /// `time` is the timestamp to be used for all node times.
    ///
    /// `uid` and `gid` indicate the ownership details of the node.  These should always match the
    /// values of the currently-running process -- but not necessarily if we want to let users
    /// customize these via flags at some point.
    pub fn new_root(time: Timespec, uid: unistd::Uid, gid: unistd::Gid) -> Arc<Node> {
        let inode = fuse::FUSE_ROOT_ID;

        #[cfg_attr(feature = "cargo-clippy", allow(redundant_field_names))]
        let attr = fuse::FileAttr {
            ino: inode,
            kind: fuse::FileType::Directory,
            nlink: 2,  // "." entry plus whichever initial named node points at this.
            size: 2,  // TODO(jmmv): Reevaluate what directory sizes should be.
            blocks: 1,  // TODO(jmmv): Reevaluate what directory blocks should be.
            atime: time,
            mtime: time,
            ctime: time,
            crtime: time,
            perm: 0o555 as u16,  // Scaffold directories cannot be mutated by the user.
            uid: uid.as_raw(),
            gid: gid.as_raw(),
            rdev: 0,
            flags: 0,
        };

        let state = MutableDir {
            parent: inode,
            underlying_path: None,
            attr,
        };

        Arc::new(Dir {
            inode,
            writable: false,
            state: Mutex::from(state),
        })
    }

    /// Creates a new directory whose contents are backed by another directory.
    ///
    /// `inode` is the node number to assign to the created in-memory directory and has no relation
    /// to the underlying directory.  `underlying_path` indicates the path to the directory outside
    /// of the sandbox that backs this one.  `fs_attr` contains the stat data for the given path.
    ///
    /// `fs_attr` is an input parameter because, by the time we decide to instantiate a directory
    /// node (e.g. as we discover directory entries during readdir or lookup), we have already
    /// issued a stat on the underlying file system and we cannot re-do it for efficiency reasons.
    pub fn new_mapped(inode: u64, underlying_path: &Path, fs_attr: &fs::Metadata, writable: bool)
        -> Arc<Node> {
        if !fs_attr.is_dir() {
            panic!("Can only construct based on dirs");
        }
        let attr = conv::attr_fs_to_fuse(underlying_path, inode, &fs_attr);

        let state = MutableDir {
            parent: inode,
            underlying_path: Some(PathBuf::from(underlying_path)),
            attr,
        };

        Arc::new(Dir { inode, writable, state: Mutex::from(state) })
    }
}

impl Node for Dir {
    fn inode(&self) -> u64 {
        self.inode
    }

    fn writable(&self) -> bool {
        self.writable
    }

    fn getattr(&self) -> NodeResult<fuse::FileAttr> {
        let mut state = self.state.lock().unwrap();

        let check_type = |t: fs::FileType| if t.is_dir() { Ok(()) } else { Err("directory") };
        if let Some(attr) = super::get_new_attr(
            self.inode, state.underlying_path.as_ref(), check_type)? {
            state.attr = attr;
        }

        Ok(state.attr)
    }

    fn lookup(&self, _name: &OsStr) -> NodeResult<(Arc<Node>, fuse::FileAttr)> {
        Err(KernelError::from_errno(errno::Errno::ENOENT))
    }

    fn readdir(&self, reply: &mut fuse::ReplyDirectory) -> NodeResult<()> {
        let state = self.state.lock().unwrap();

        reply.add(self.inode, 0, fuse::FileType::Directory, ".");
        reply.add(state.parent, 1, fuse::FileType::Directory, "..");
        Ok(())
    }
}
