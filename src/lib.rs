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

#![cfg_attr(feature = "cargo-clippy", allow(redundant_field_names))]

#[macro_use] extern crate failure;
extern crate fuse;
#[macro_use] extern crate log;
extern crate nix;
#[cfg(test)] extern crate tempdir;
extern crate time;

use nix::{errno, unistd};
use std::collections::HashMap;
use std::ffi::OsStr;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::result::Result;
use std::sync::{Arc, Mutex};
use std::sync::atomic::{AtomicUsize, Ordering};
use time::Timespec;

mod nodes;
#[cfg(test)] mod testutils;

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

/// Monotonically-increasing generator of identifiers.
pub struct IdGenerator {
    last_id: AtomicUsize,
}

impl IdGenerator {
    /// Constructs a new generator that starts at the given value.
    fn new(start_value: u64) -> Self {
        IdGenerator { last_id: AtomicUsize::new(start_value as usize) }
    }

    /// Obtains a new identifier.
    pub fn next(&self) -> u64 {
        let id = self.last_id.fetch_add(1, Ordering::AcqRel);
        // TODO(https://github.com/rust-lang/rust/issues/51577): Drop :: prefix.
        if id >= ::std::u64::MAX as usize {
            panic!("Ran out of identifiers");
        }
        id as u64
    }
}

/// Cache of sandboxfs nodes indexed by their underlying path.
///
/// This cache is critical to offer good performance during reconfigurations: if the identity of an
/// underlying file changes across reconfigurations, the kernel will think it's a different file
/// (even if it may not be) and will therefore not be able to take advantage of any caches.  You
/// would think that avoiding kernel cache invalidations during the reconfiguration itself (e.g. if
/// file `A` was mapped and is still mapped now, don't invalidate it) would be sufficient to avoid
/// this problem, but it's not: `A` could be mapped, then unmapped, and then remapped again in three
/// different reconfigurations, and we'd still not want to lose track of it.
///
/// Nodes should be inserted in this cache at creation time and removed from it when explicitly
/// deleted by the user (because there is a chance they'll be recreated, and at that point we truly
/// want to reload the data from disk).
///
/// TODO(jmmv): There currently is no cache expiration, which means that memory usage can grow
/// unboundedly.  A preliminary attempt at expiring cache entries on a node's forget handler sounded
/// promising (because then cache expiration would be delegated to the kernel)... but, on Linux, the
/// kernel seems to be calling this very eagerly, rendering our cache useless.  I did not track down
/// what exactly triggered the forget notifications though.
#[derive(Default)]
pub struct Cache {
    entries: Mutex<HashMap<PathBuf, Arc<nodes::Node>>>,
}

impl Cache {
    /// Gets a mapped node from the cache or creates a new one if not yet cached.
    ///
    /// The returned node represents the given underlying path uniquely.  If creation is needed, the
    /// created node uses the given type and writable settings.
    pub fn get_or_create(&self, ids: &IdGenerator, underlying_path: &Path, attr: &fs::Metadata,
        writable: bool) -> Arc<nodes::Node> {
        if attr.is_dir() {
            // Directories cannot be cached because they contain entries that are created only
            // in memory based on the mappings configuration.
            //
            // TODO(jmmv): Actually, they *could* be cached, but it's hard.  Investigate doing so
            // after quantifying how much it may benefit performance.
            return nodes::Dir::new_mapped(ids.next(), underlying_path, attr, writable);
        }

        let mut entries = self.entries.lock().unwrap();

        if let Some(node) = entries.get(underlying_path) {
            if node.writable() == writable {
                // We have a match from the cache!  Return it immediately.
                //
                // It is tempting to ensure that the type of the cached node matches the type we
                // want to return based on the metadata we have now in `attr`... but doing so does
                // not really prevent problems: the type of the underlying file can change at any
                // point in time.  We could check this here and the type could change immediately
                // afterwards behind our backs, so don't bother.
                return node.clone();
            }

            // We had a match... but node writability has changed; recreate the node.
            //
            // You may wonder why we care about this and not the file type as described above: the
            // reason is that the writability property is a setting of the mappings, not a property
            // of the underlying files, and thus it's a setting that we fully control and must keep
            // correct across reconfigurations or across different mappings of the same files.
            info!("Missed node caching opportunity because writability has changed for {:?}",
                underlying_path)
        }

        let node: Arc<nodes::Node> = if attr.is_dir() {
            panic!("Directory entries cannot be cached and are handled above");
        } else if attr.file_type().is_symlink() {
            nodes::Symlink::new_mapped(ids.next(), underlying_path, attr, writable)
        } else {
            nodes::File::new_mapped(ids.next(), underlying_path, attr, writable)
        };
        entries.insert(underlying_path.to_path_buf(), node.clone());
        node
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
        let ids = IdGenerator::new(fuse::FUSE_ROOT_ID);
        let cache = Cache::default();

        let root = {
            if mappings.is_empty() {
                let now = time::get_time();
                nodes::Dir::new_root(now, unistd::getuid(), unistd::getgid())
            } else if mappings.len() == 1 {
                if mappings[0].path != Path::new("/") {
                    panic!("Unimplemented; only support a single mapping for the root directory");
                }
                let fs_attr = fs::symlink_metadata(&mappings[0].underlying_path)?;
                if !fs_attr.is_dir() {
                    warn!("Path {:?} is not a directory; got {:?}", &mappings[0].underlying_path,
                        &fs_attr);
                    return Err(io::Error::from_raw_os_error(errno::Errno::EIO as i32));
                }
                cache.get_or_create(&ids, &mappings[0].underlying_path, &fs_attr,
                    mappings[0].writable)
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
            Err(e) => reply.error(e.errno_as_i32()),
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
            Err(e) => reply.error(e.errno_as_i32()),
        }
    }

    fn readdir(&mut self, _req: &fuse::Request, inode: u64, _handle: u64, offset: i64,
               mut reply: fuse::ReplyDirectory) {
        if offset == 0 {
            let node = self.find_node(inode);
            match node.readdir(&mut reply) {
                Ok(()) => reply.ok(),
                Err(e) => reply.error(e.errno_as_i32()),
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
    use tempdir::TempDir;

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

    #[test]
    fn id_generator_ok() {
        let ids = IdGenerator::new(10);
        assert_eq!(10, ids.next());
        assert_eq!(11, ids.next());
        assert_eq!(12, ids.next());
    }

    #[test]
    #[should_panic(expected = "Ran out of identifiers")]
    fn id_generator_exhaustion() {
        let ids = IdGenerator::new(std::u64::MAX);
        ids.next();  // OK, still at limit.
        ids.next();  // Should panic.
    }

    #[test]
    fn cache_behavior() {
        let tempdir = TempDir::new("test").unwrap();

        let dir1 = tempdir.path().join("dir1");
        fs::create_dir(&dir1).unwrap();
        let dir1attr = fs::symlink_metadata(&dir1).unwrap();

        let file1 = tempdir.path().join("file1");
        drop(fs::File::create(&file1).unwrap());
        let file1attr = fs::symlink_metadata(&file1).unwrap();

        let file2 = tempdir.path().join("file2");
        drop(fs::File::create(&file2).unwrap());
        let file2attr = fs::symlink_metadata(&file2).unwrap();

        let ids = IdGenerator::new(1);
        let cache = Cache::default();

        // Directories are not cached no matter what.
        assert_eq!(1, cache.get_or_create(&ids, &dir1, &dir1attr, false).inode());
        assert_eq!(2, cache.get_or_create(&ids, &dir1, &dir1attr, false).inode());
        assert_eq!(3, cache.get_or_create(&ids, &dir1, &dir1attr, true).inode());

        // Different files get different nodes.
        assert_eq!(4, cache.get_or_create(&ids, &file1, &file1attr, false).inode());
        assert_eq!(5, cache.get_or_create(&ids, &file2, &file2attr, true).inode());

        // Files we queried before but with different writability get different nodes.
        assert_eq!(6, cache.get_or_create(&ids, &file1, &file1attr, true).inode());
        assert_eq!(7, cache.get_or_create(&ids, &file2, &file2attr, false).inode());

        // We get cache hits when everything matches previous queries.
        assert_eq!(6, cache.get_or_create(&ids, &file1, &file1attr, true).inode());
        assert_eq!(7, cache.get_or_create(&ids, &file2, &file2attr, false).inode());

        // We don't get cache hits for nodes whose writability changed.
        assert_eq!(8, cache.get_or_create(&ids, &file1, &file1attr, false).inode());
        assert_eq!(9, cache.get_or_create(&ids, &file2, &file2attr, true).inode());
    }

    #[test]
    fn cache_nodes_support_all_file_types() {
        let ids = IdGenerator::new(1);
        let cache = Cache::default();

        for (_fuse_type, path) in testutils::AllFileTypes::new().entries {
            let fs_attr = fs::symlink_metadata(&path).unwrap();
            // The following panics if it's impossible to represent the given file type, which is
            // what we are testing.
            cache.get_or_create(&ids, &path, &fs_attr, false);
        }
    }
}
