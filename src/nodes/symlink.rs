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

use nix::errno;
use nodes::{AttrDelta, KernelError, Node, NodeResult, conv, setattr};
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

/// Representation of a symlink node.
pub struct Symlink {
    inode: u64,
    writable: bool,
    state: Mutex<MutableSymlink>,
}

/// Holds the mutable data of a symlink node.
struct MutableSymlink {
    underlying_path: PathBuf,
    attr: fuse::FileAttr,
}

impl Symlink {
    /// Creates a new symlink backed by a symlink on an underlying file system.
    ///
    /// `inode` is the node number to assign to the created in-memory symlink and has no relation
    /// to the underlying symlink.  `underlying_path` indicates the path to the symlink outside
    /// of the sandbox that backs this one.  `fs_attr` contains the stat data for the given path.
    ///
    /// `fs_attr` is an input parameter because, by the time we decide to instantiate a symlink
    /// node (e.g. as we discover directory entries during readdir or lookup), we have already
    /// issued a stat on the underlying file system and we cannot re-do it for efficiency reasons.
    pub fn new_mapped(inode: u64, underlying_path: &Path, fs_attr: &fs::Metadata, writable: bool)
        -> Arc<Node> {
        if !fs_attr.file_type().is_symlink() {
            panic!("Can only construct based on symlinks");
        }
        let attr = conv::attr_fs_to_fuse(underlying_path, inode, &fs_attr);

        let state = MutableSymlink {
            underlying_path: PathBuf::from(underlying_path),
            attr,
        };

        Arc::new(Symlink { inode, writable, state: Mutex::from(state) })
    }

    /// Same as `getattr` but with the node already locked.
    fn getattr_unlocked(inode: u64, state: &mut MutableSymlink) -> NodeResult<fuse::FileAttr> {
        let fs_attr = fs::symlink_metadata(&state.underlying_path)?;
        if !fs_attr.file_type().is_symlink() {
            warn!("Path {:?} backing a symlink node is no longer a symlink; got {:?}",
                &state.underlying_path, fs_attr.file_type());
            return Err(KernelError::from_errno(errno::Errno::EIO));
        }
        state.attr = conv::attr_fs_to_fuse(&state.underlying_path, inode, &fs_attr);

        Ok(state.attr)
    }
}

impl Node for Symlink {
    fn inode(&self) -> u64 {
        self.inode
    }

    fn writable(&self) -> bool {
        self.writable
    }

    fn file_type_cached(&self) -> fuse::FileType {
        fuse::FileType::Symlink
    }

    fn getattr(&self) -> NodeResult<fuse::FileAttr> {
        let mut state = self.state.lock().unwrap();
        Symlink::getattr_unlocked(self.inode, &mut state)
    }

    fn readlink(&self) -> NodeResult<PathBuf> {
        let state = self.state.lock().unwrap();

        Ok(fs::read_link(&state.underlying_path)?)
    }

    fn setattr(&self, delta: &AttrDelta) -> NodeResult<fuse::FileAttr> {
        let mut state = self.state.lock().unwrap();
        setattr(&state.underlying_path, &state.attr, delta)?;
        Symlink::getattr_unlocked(self.inode, &mut state)
    }
}
