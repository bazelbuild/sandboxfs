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
use nix::sys;
use nix::sys::time::TimeValLike;
use std::fs;
use std::io;
use std::os::unix::fs::{FileTypeExt, MetadataExt, PermissionsExt};
use std::path::Path;
use std::time::{SystemTime, UNIX_EPOCH};
use time::Timespec;

/// Fixed point in time to use when we fail to interpret file system supplied timestamps.
const BAD_TIME: Timespec = Timespec { sec: 0, nsec: 0 };

/// Converts a system time as represented in a fs::Metadata object to a Timespec.
///
/// `path` is the file from which the timestamp was originally extracted and `name` represents the
/// metadata field the timestamp corresponds to; both are used for debugging purposes only.
///
/// If the given system time is missing, or if it is invalid, logs a warning and returns a fixed
/// time.  It is reasonable for these details to be missing because the backing file systems do
/// not always implement all possible file timestamps.
fn system_time_to_timespec(path: &Path, name: &str, time: &io::Result<SystemTime>) -> Timespec {
    match time {
        Ok(time) => match time.duration_since(UNIX_EPOCH) {
            Ok(duration) => Timespec::new(duration.as_secs() as i64,
                                          duration.subsec_nanos() as i32),
            Err(e) => {
                warn!("File system returned {} {:?} for {:?} that's before the Unix epoch: {}",
                      name, time, path, e);
                BAD_TIME
            }
        },
        Err(e) => {
            warn!("File system did not return a {} timestamp for {:?}: {}", name, path, e);
            BAD_TIME
        },
    }
}

/// Converts a `time::Timespec` object into a `sys::time::TimeVal`.
// TODO(jmmv): Consider upstreaming this function or a constructor for TimeVal that takes the two
// components separately.
pub fn timespec_to_timeval(spec: Timespec) -> sys::time::TimeVal {
    use sys::time::TimeVal;
    TimeVal::seconds(spec.sec) + TimeVal::nanoseconds(spec.nsec.into())
}

/// Converts a file type as returned by the file system to a FUSE file type.
///
/// `path` is the file from which the file type was originally extracted and is only for debugging
/// purposes.
///
/// If the given file type cannot be mapped to a FUSE file type (because we don't know about that
/// type or, most likely, because the file type is bogus), logs a warning and returns a regular
/// file type with the assumption that most operations should work on it.
pub fn filetype_fs_to_fuse(path: &Path, fs_type: fs::FileType) -> fuse::FileType {
    if fs_type.is_block_device() {
        fuse::FileType::BlockDevice
    } else if fs_type.is_char_device() {
        fuse::FileType::CharDevice
    } else if fs_type.is_dir() {
        fuse::FileType::Directory
    } else if fs_type.is_fifo() {
        fuse::FileType::NamedPipe
    } else if fs_type.is_file() {
        fuse::FileType::RegularFile
    } else if fs_type.is_socket() {
        fuse::FileType::Socket
    } else if fs_type.is_symlink() {
        fuse::FileType::Symlink
    } else {
        warn!("File system returned invalid file type {:?} for {:?}", fs_type, path);
        fuse::FileType::RegularFile
    }
}

/// Converts metadata attributes supplied by the file system to a FUSE file attributes tuple.
///
/// `inode` is the value of the FUSE inode (not the value of the inode supplied within `attr`) to
/// fill into the returned file attributes.  `path` is the file from which the attributes were
/// originally extracted and is only for debugging purposes.
///
/// Any errors encountered along the conversion process are logged and the corresponding field is
/// replaced by a reasonable value that should work.  In other words: all errors are swallowed.
pub fn attr_fs_to_fuse(path: &Path, inode: u64, attr: &fs::Metadata) -> fuse::FileAttr {
    let nlink = if attr.is_dir() {
        2  // "." entry plus whichever initial named node points at this.
    } else {
        1  // We don't support hard links so this is always valid.
    };

    let len = if attr.is_dir() {
        2  // TODO(jmmv): Reevaluate what directory sizes should be.
    } else {
        attr.len()
    };

    // TODO(https://github.com/bazelbuild/sandboxfs/issues/43): Using the underlying ctimes is
    // slightly wrong because the ctimes track changes to the inodes.  In most cases, operations
    // that flow via sandboxfs will affect the underlying ctime and propagate through here, which is
    // fine, but other operations are purely in-memory.  To properly handle those cases, we should
    // have our own ctime handling.
    let ctime = Timespec { sec: attr.ctime(), nsec: attr.ctime_nsec() as i32 };

    let perm = match attr.permissions().mode() {
        // TODO(https://github.com/rust-lang/rust/issues/51577): Drop :: prefix.
        mode if mode > u32::from(::std::u16::MAX) => {
            warn!("File system returned mode {} for {:?}, which is too large; set to 0400",
                mode, path);
            0o400
        },
        mode => (mode as u16) & !(sys::stat::SFlag::S_IFMT.bits() as u16),
    };

    let rdev = match attr.rdev() {
        // TODO(https://github.com/rust-lang/rust/issues/51577): Drop :: prefix.
        rdev if rdev > u64::from(::std::u32::MAX) => {
            warn!("File system returned rdev {} for {:?}, which is too large; set to 0",
                rdev, path);
            0
        },
        rdev => rdev as u32,
    };

    fuse::FileAttr {
        ino: inode,
        kind: filetype_fs_to_fuse(path, attr.file_type()),
        nlink: nlink,
        size: len,
        blocks: 0, // TODO(jmmv): Reevaluate what blocks should be.
        atime: system_time_to_timespec(path, "atime", &attr.accessed()),
        mtime: system_time_to_timespec(path, "mtime", &attr.modified()),
        ctime: ctime,
        crtime: system_time_to_timespec(path, "crtime", &attr.created()),
        perm: perm,
        uid: attr.uid(),
        gid: attr.gid(),
        rdev: rdev,
        flags: 0,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    use nix::{errno, unistd};
    use nix::sys::time::TimeValLike;
    use std::fs::File;
    use std::io::Write;
    use std::time::Duration;
    use tempdir::TempDir;
    use testutils;

    #[test]
    fn test_timespec_to_timeval() {
        let spec = Timespec { sec: 123, nsec: 45000 };
        let val = timespec_to_timeval(spec);
        assert_eq!(123, val.tv_sec());
        assert_eq!(45, val.tv_usec());
    }

    #[test]
    fn test_system_time_to_timespec_ok() {
        let sys_time = SystemTime::UNIX_EPOCH + Duration::new(12345, 6789);
        let timespec = system_time_to_timespec(
            &Path::new("irrelevant"), "irrelevant", &Ok(sys_time));
        assert_eq!(Timespec { sec: 12345, nsec: 6789 }, timespec);
    }

    #[test]
    fn test_system_time_to_timespec_bad() {
        let sys_time = SystemTime::UNIX_EPOCH - Duration::new(1, 0);
        let timespec = system_time_to_timespec(
            &Path::new("irrelevant"), "irrelevant", &Ok(sys_time));
        assert_eq!(BAD_TIME, timespec);
    }

    #[test]
    fn test_system_time_to_timespec_missing() {
        let timespec = system_time_to_timespec(
            &Path::new("irrelevant"), "irrelevant",
            &Err(io::Error::from_raw_os_error(errno::Errno::ENOENT as i32)));
        assert_eq!(BAD_TIME, timespec);
    }

    #[test]
    fn test_filetype_fs_to_fuse() {
        let files = testutils::AllFileTypes::new();
        for (exp_type, path) in files.entries {
            let fs_type = fs::symlink_metadata(&path).unwrap().file_type();
            assert_eq!(exp_type, filetype_fs_to_fuse(&path, fs_type));
        }
    }

    #[test]
    fn test_attr_fs_to_fuse_directory() {
        let dir = TempDir::new("test").unwrap();
        let path = dir.path().join("root");
        fs::create_dir(&path).unwrap();
        fs::create_dir(path.join("subdir1")).unwrap();
        fs::create_dir(path.join("subdir2")).unwrap();

        fs::set_permissions(&path, fs::Permissions::from_mode(0o750)).unwrap();
        sys::stat::utimes(&path, &sys::time::TimeVal::seconds(12345),
            &sys::time::TimeVal::seconds(678)).unwrap();

        let exp_attr = fuse::FileAttr {
            ino: 1234,  // Ensure underlying inode is not propagated.
            kind: fuse::FileType::Directory,
            nlink: 2, // TODO(jmmv): Should account for subdirs.
            size: 2,
            blocks: 0,
            atime: Timespec { sec: 12345, nsec: 0 },
            mtime: Timespec { sec: 678, nsec: 0 },
            ctime: BAD_TIME,
            crtime: BAD_TIME,
            perm: 0o750,
            uid: unistd::getuid().as_raw(),
            gid: unistd::getgid().as_raw(),
            rdev: 0,
            flags: 0,
        };

        let mut attr = attr_fs_to_fuse(&path, 1234, &fs::symlink_metadata(&path).unwrap());
        // We cannot really make any useful assertions on ctime and crtime as these cannot be
        // modified and may not be queryable, so stub them out.
        attr.ctime = BAD_TIME;
        attr.crtime = BAD_TIME;
        assert_eq!(&exp_attr, &attr);
    }

    #[test]
    fn test_attr_fs_to_fuse_regular() {
        let dir = TempDir::new("test").unwrap();
        let path = dir.path().join("file");

        let mut file = File::create(&path).unwrap();
        let content = "Some text\n";
        file.write_all(content.as_bytes()).unwrap();
        drop(file);

        fs::set_permissions(&path, fs::Permissions::from_mode(0o640)).unwrap();
        sys::stat::utimes(&path, &sys::time::TimeVal::seconds(54321),
            &sys::time::TimeVal::seconds(876)).unwrap();

        let exp_attr = fuse::FileAttr {
            ino: 42,  // Ensure underlying inode is not propagated.
            kind: fuse::FileType::RegularFile,
            nlink: 1,
            size: content.len() as u64,
            blocks: 0,
            atime: Timespec { sec: 54321, nsec: 0 },
            mtime: Timespec { sec: 876, nsec: 0 },
            ctime: BAD_TIME,
            crtime: BAD_TIME,
            perm: 0o640,
            uid: unistd::getuid().as_raw(),
            gid: unistd::getgid().as_raw(),
            rdev: 0,
            flags: 0,
        };

        let mut attr = attr_fs_to_fuse(&path, 42, &fs::symlink_metadata(&path).unwrap());
        // We cannot really make any useful assertions on ctime and crtime as these cannot be
        // modified and may not be queryable, so stub them out.
        attr.ctime = BAD_TIME;
        attr.crtime = BAD_TIME;
        assert_eq!(&exp_attr, &attr);
    }
}
