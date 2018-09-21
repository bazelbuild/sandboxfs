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

#[macro_use] extern crate failure;
extern crate fuse;
extern crate libc;
#[macro_use] extern crate log;
#[cfg(test)] extern crate tempdir;
extern crate time;

use std::collections::HashMap;
use std::ffi::OsStr;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::result::Result;
use std::sync::{Arc, Mutex};
use time::Timespec;

mod nodes;

// TODO(jmmv): Make configurable via a flag and store inside SandboxFS.
pub const TTL: Timespec = Timespec { sec: 60, nsec: 0 };

/// An error indicating that a path has to be absolute but isn't.
#[derive(Debug, Fail)]
#[fail(display = "path {:?} is not absolute", path)]
pub struct PathNotAbsoluteError {
    /// The path that caused this error.
    pub path: PathBuf,
}

/// Mapping describes how an individual path within the sandbox is connected to an external path
/// in the underlying file system.
#[derive(Debug, Eq, PartialEq)]
pub struct Mapping {
    path: PathBuf,
    underlying_path: PathBuf,
    writable: bool,
}

impl Mapping {
    /// Creates a new mapping from the individual components.
    ///
    /// `path` is the inside the sandbox's mount point where the `underlying_path` is exposed.
    /// Both must be absolute paths.
    pub fn new(path: PathBuf, underlying_path: PathBuf, writable: bool)
        -> Result<Mapping, PathNotAbsoluteError> {
        if !path.is_absolute() {
            return Err(PathNotAbsoluteError { path });
        }
        if !underlying_path.is_absolute() {
            return Err(PathNotAbsoluteError { path: underlying_path });
        }

        Ok(Mapping { path, underlying_path, writable })
    }
}

/// FUSE file system implementation of sandboxfs.
struct SandboxFS {
    /// Mapping of inode numbers to in-memory nodes that tracks all files known by sandboxfs.
    nodes: Arc<Mutex<HashMap<u64, Arc<nodes::Node>>>>,
}

impl SandboxFS {
    /// Creates a new `SandboxFS` instance.
    fn new(mappings: &[Mapping]) -> io::Result<SandboxFS> {
        let root = {
            if mappings.is_empty() {
                let now = time::get_time();
                let uid = unsafe { libc::getuid() } as u32;
                let gid = unsafe { libc::getgid() } as u32;
                nodes::Dir::new_root(now, uid, gid)
            } else if mappings.len() == 1 {
                if mappings[0].path != Path::new("/") {
                    panic!("Unimplemented; only support a single mapping for the root directory");
                }
                let fs_attr = fs::symlink_metadata(&mappings[0].underlying_path)?;
                if !fs_attr.is_dir() {
                    warn!("Path {:?} is not a directory; got {:?}", &mappings[0].underlying_path,
                        &fs_attr);
                    return Err(io::Error::from_raw_os_error(libc::EIO));
                }
                nodes::Dir::new_mapped(fuse::FUSE_ROOT_ID, &mappings[0].underlying_path, &fs_attr)
            } else {
                panic!("Unimplemented; only support zero or one mappings so far");
            }
        };

        let mut nodes = HashMap::new();
        assert_eq!(fuse::FUSE_ROOT_ID, root.inode());
        nodes.insert(root.inode(), root);

        Ok(SandboxFS {
            nodes: Arc::from(Mutex::from(nodes)),
        })
    }

    /// Gets a node given its `inode`.
    ///
    /// We assume that the inode number is valid and that we have a known node for it; otherwise,
    /// we crash.  The rationale for this is that this function is always called on inode numbers
    /// requested by the kernel, and we can trust that the kernel will only ever ask us for inode
    /// numbers we have previously told it about.
    fn find_node(&mut self, inode: u64) -> Arc<nodes::Node> {
        let nodes = self.nodes.lock().unwrap();
        match nodes.get(&inode) {
            Some(node) => node.clone(),
            None => panic!("Kernel requested unknown inode {}", inode),
        }
    }
}

impl fuse::Filesystem for SandboxFS {
    fn getattr(&mut self, _req: &fuse::Request, inode: u64, reply: fuse::ReplyAttr) {
        let node = self.find_node(inode);
        match node.getattr() {
            Ok(attr) => reply.attr(&TTL, &attr),
            Err(e) => reply.error(e.errno()),
        }
    }

    fn lookup(&mut self, _req: &fuse::Request, parent: u64, name: &OsStr, reply: fuse::ReplyEntry) {
        let dir_node = self.find_node(parent);
        match dir_node.lookup(name) {
            Ok((node, attr)) => {
                {
                    let mut nodes = self.nodes.lock().unwrap();
                    if !nodes.contains_key(&node.inode()) {
                        nodes.insert(node.inode(), node);
                    }
                }
                reply.entry(&TTL, &attr, 0);
            },
            Err(e) => reply.error(e.errno()),
        }
    }

    fn readdir(&mut self, _req: &fuse::Request, inode: u64, _handle: u64, offset: i64,
               mut reply: fuse::ReplyDirectory) {
        if offset == 0 {
            let node = self.find_node(inode);
            match node.readdir(&mut reply) {
                Ok(()) => reply.ok(),
                Err(e) => reply.error(e.errno()),
            }
        } else {
            assert!(offset > 0, "Do not know what to do with a negative offset");
            // Our node.readdir() implementation reads the whole directory in one go.  Therefore,
            // if we get an offset different than zero, it's because the kernel has already
            // completed the first read and is asking us for extra entries -- of which there will
            // be none.
            reply.ok();
        }
    }
}

/// Mounts a new sandboxfs instance on the given `mount_point` and maps all `mappings` within it.
pub fn mount(mount_point: &Path, mappings: &[Mapping]) -> io::Result<()> {
    let options = ["-o", "ro", "-o", "fsname=sandboxfs"]
        .iter()
        .map(|o| o.as_ref())
        .collect::<Vec<&OsStr>>();
    let fs = SandboxFS::new(mappings)?;
    info!("Mounting file system onto {:?}", mount_point);
    fuse::mount(fs, &mount_point, &options)
        .map_err(|e| io::Error::new(e.kind(), format!("mount on {:?} failed: {}", mount_point, e)))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_mapping_new_ok() {
        let mapping = Mapping::new(PathBuf::from("/foo"), PathBuf::from("/bar"), false).unwrap();
        assert_eq!(Path::new("/foo"), mapping.path);
        assert_eq!(Path::new("/bar"), mapping.underlying_path);
        assert!(!mapping.writable);
    }

    #[test]
    fn test_mapping_new_bad_path() {
        let err = Mapping::new(PathBuf::from("foo"), PathBuf::from("/bar"), false).unwrap_err();
        assert_eq!(Path::new("foo"), err.path);
    }

    #[test]
    fn test_mapping_new_bad_underlying_path() {
        let err = Mapping::new(PathBuf::from("/foo"), PathBuf::from("bar"), false).unwrap_err();
        assert_eq!(Path::new("bar"), err.path);
    }
}
