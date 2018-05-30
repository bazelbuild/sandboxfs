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
#[macro_use] extern crate log;

use std::ffi::OsStr;
use std::io;
use std::path::Path;

/// FUSE file system implementation of sandboxfs.
struct SandboxFS {
}

impl SandboxFS {
    /// Creates a new `SandboxFS` instance.
    fn new() -> SandboxFS {
      SandboxFS {}
    }
}

impl fuse::Filesystem for SandboxFS {
}

/// Mounts a new sandboxfs instance on the given `mount_point`.
pub fn mount(mount_point: &Path) -> io::Result<()> {
    let options = ["-o", "ro", "-o", "fsname=sandboxfs"]
        .iter()
        .map(|o| o.as_ref())
        .collect::<Vec<&OsStr>>();
    let fs = SandboxFS::new();
    info!("Mounting file system onto {:?}", mount_point);
    fuse::mount(fs, &mount_point, &options)
        .map_err(|e| io::Error::new(
            e.kind(), format!("mount on {:?} failed: {}", mount_point, e)))
}
