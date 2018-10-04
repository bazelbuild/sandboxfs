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

use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use super::conv;
use super::Node;
use super::NodeResult;

/// Representation of a symlink node.
pub struct Symlink {
    inode: u64,
    writable: bool,
    state: Mutex<MutableSymlink>,
}

/// Holds the mutable data of a symlink node.
struct MutableSymlink {
    underlying_path: Option<PathBuf>,
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
            underlying_path: Some(PathBuf::from(underlying_path)),
            attr,
        };

        Arc::new(Symlink { inode, writable, state: Mutex::from(state) })
    }
}

impl Node for Symlink {
    fn inode(&self) -> u64 {
        self.inode
    }

    fn writable(&self) -> bool {
        self.writable
    }

    fn getattr(&self) -> NodeResult<fuse::FileAttr> {
        let mut state = self.state.lock().unwrap();

        let check_type = |t: fs::FileType| if t.is_symlink() { Ok(()) } else { Err("symlink") };
        if let Some(attr) = super::get_new_attr(
            self.inode, state.underlying_path.as_ref(), check_type)? {
            state.attr = attr;
        };

        Ok(state.attr)
    }
}
