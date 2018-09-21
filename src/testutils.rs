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

#![cfg(test)]

use fuse;
use libc;
use std::collections::HashMap;
use std::ffi::CString;
use std::fs;
use std::os::unix;
use std::path::{Path, PathBuf};
use tempdir::TempDir;

/// Creates a block or character device and enforces that it is created successfully.
fn mkdev(path: &Path, type_mask: libc::mode_t, dev: libc::dev_t) {
    assert!(type_mask == libc::S_IFBLK || type_mask == libc::S_IFCHR);
    let path = path.as_os_str().to_str().unwrap().as_bytes();
    let path = CString::new(path).unwrap();
    assert_eq!(0, unsafe { libc::mknod(path.as_ptr(), 0o444 | type_mask, dev) });
}

/// Creates a named pipe and enforces that it is created successfully.
fn mkfifo(path: &Path) {
    let path = path.as_os_str().to_str().unwrap().as_bytes();
    let path = CString::new(path).unwrap();
    assert_eq!(0, unsafe { libc::mkfifo(path.as_ptr(), 0o444) });
}

/// Holds a temporary directory and files of all possible kinds within it.
///
/// The directory (including all of its contents) is removed when this object is dropped.
pub struct AllFileTypes {
    #[allow(unused)]  // Must retain to delay directory deletion.
    root: TempDir,

    /// Collection of test files keyed by their type.
    ///
    /// Tests should iterate over this map and consume all entries to ensure all possible file types
    /// are verified everywhere.  Prefer using `match` on the key to achieve this.
    pub entries: HashMap<fuse::FileType, PathBuf>,
}

impl AllFileTypes {
    /// Creates a new temporary directory with files of all possible kinds within it.
    pub fn new() -> Self {
        let root = TempDir::new("test").unwrap();

        let mut entries: HashMap<fuse::FileType, PathBuf> = HashMap::new();

        if unsafe { libc::getuid() } == 0 {
            let block_device = root.path().join("block_device");
            mkdev(&block_device, libc::S_IFBLK, 50);
            entries.insert(fuse::FileType::BlockDevice, block_device);

            let char_device = root.path().join("char_device");
            mkdev(&char_device, libc::S_IFCHR, 50);
            entries.insert(fuse::FileType::CharDevice, char_device);
        } else {
            warn!("Not running as root; cannot create block/char devices");
        }

        let directory = root.path().join("dir");
        fs::create_dir(&directory).unwrap();
        entries.insert(fuse::FileType::Directory, directory);

        let named_pipe = root.path().join("named_pipe");
        mkfifo(&named_pipe);
        entries.insert(fuse::FileType::NamedPipe, named_pipe);

        let regular = root.path().join("regular");
        drop(fs::File::create(&regular).unwrap());
        entries.insert(fuse::FileType::RegularFile, regular);

        let socket = root.path().join("socket");
        drop(unix::net::UnixListener::bind(&socket).unwrap());
        entries.insert(fuse::FileType::Socket, socket);

        let symlink = root.path().join("symlink");
        unix::fs::symlink("irrelevant", &symlink).unwrap();
        entries.insert(fuse::FileType::Symlink, symlink);

        AllFileTypes { root, entries }
    }
}
