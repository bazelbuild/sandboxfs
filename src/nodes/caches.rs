// Copyright 2020 Google Inc.
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

use {fuse, IdGenerator};
use nodes::{ArcNode, Cache, Dir, File, Symlink};
use std::collections::HashMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

/// Node factory without any caching.
#[derive(Default)]
pub struct NoCache {
}

impl Cache for NoCache {
    fn get_or_create(&self, ids: &IdGenerator, underlying_path: &Path, attr: &fs::Metadata,
        writable: bool) -> ArcNode {
        if attr.is_dir() {
            Dir::new_mapped(ids.next(), underlying_path, attr, writable)
        } else if attr.file_type().is_symlink() {
            Symlink::new_mapped(ids.next(), underlying_path, attr, writable)
        } else {
            File::new_mapped(ids.next(), underlying_path, attr, writable)
        }
    }

    fn delete(&self, _path: &Path, _file_type: fuse::FileType) {
        // Nothing to do.
    }

    fn rename(&self, _old_path: &Path, _new_path: PathBuf, _file_type: fuse::FileType) {
        // Nothing to do.
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
///
/// TODO(jmmv): This cache has proven to be problematic in some cases and should probably be
/// removed.  See https://jmmv.dev/2020/01/osxfuse-hardlinks-dladdr.html for details.
#[derive(Default)]
pub struct PathCache {
    entries: Mutex<HashMap<PathBuf, ArcNode>>,
}

impl Cache for PathCache {
    fn get_or_create(&self, ids: &IdGenerator, underlying_path: &Path, attr: &fs::Metadata,
        writable: bool) -> ArcNode {
        if attr.is_dir() {
            // Directories cannot be cached because they contain entries that are created only
            // in memory based on the mappings configuration.
            //
            // TODO(jmmv): Actually, they *could* be cached, but it's hard.  Investigate doing so
            // after quantifying how much it may benefit performance.
            return Dir::new_mapped(ids.next(), underlying_path, attr, writable);
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

        let node: ArcNode = if attr.is_dir() {
            panic!("Directory entries cannot be cached and are handled above");
        } else if attr.file_type().is_symlink() {
            Symlink::new_mapped(ids.next(), underlying_path, attr, writable)
        } else {
            File::new_mapped(ids.next(), underlying_path, attr, writable)
        };
        entries.insert(underlying_path.to_path_buf(), node.clone());
        node
    }

    fn delete(&self, path: &Path, file_type: fuse::FileType) {
        let mut entries = self.entries.lock().unwrap();
        if file_type == fuse::FileType::Directory {
            debug_assert!(!entries.contains_key(path), "Directories are not currently cached");
        } else {
            entries.remove(path).expect("Tried to delete unknown path from the cache");
        }
    }

    fn rename(&self, old_path: &Path, new_path: PathBuf, file_type: fuse::FileType) {
        let mut entries = self.entries.lock().unwrap();
        if file_type == fuse::FileType::Directory {
            debug_assert!(!entries.contains_key(old_path), "Directories are not currently cached");
        } else {
            let node = entries.remove(old_path).expect("Tried to rename unknown path in the cache");
            entries.insert(new_path, node);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::tempdir;
    use testutils;

    #[test]
    fn path_cache_behavior() {
        let root = tempdir().unwrap();

        let dir1 = root.path().join("dir1");
        fs::create_dir(&dir1).unwrap();
        let dir1attr = fs::symlink_metadata(&dir1).unwrap();

        let file1 = root.path().join("file1");
        drop(fs::File::create(&file1).unwrap());
        let file1attr = fs::symlink_metadata(&file1).unwrap();

        let file2 = root.path().join("file2");
        drop(fs::File::create(&file2).unwrap());
        let file2attr = fs::symlink_metadata(&file2).unwrap();

        let ids = IdGenerator::new(1);
        let cache = PathCache::default();

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
    fn path_cache_nodes_support_all_file_types() {
        let ids = IdGenerator::new(1);
        let cache = PathCache::default();

        for (_fuse_type, path) in testutils::AllFileTypes::new().entries {
            let fs_attr = fs::symlink_metadata(&path).unwrap();
            // The following panics if it's impossible to represent the given file type, which is
            // what we are testing.
            cache.get_or_create(&ids, &path, &fs_attr, false);
        }
    }
}
