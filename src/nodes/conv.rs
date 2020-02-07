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
use nix::{errno, fcntl, sys};
use nix::sys::time::TimeValLike;
use nodes::{KernelError, NodeResult};
use std::fs;
use std::io;
use std::os::unix::fs::{FileTypeExt, MetadataExt, OpenOptionsExt, PermissionsExt};
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
            debug!("File system did not return a {} timestamp for {:?}: {}", name, path, e);
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

/// Converts a `sys::time::TimeVal` object into a `time::Timespec`.
// TODO(jmmv): Consider upstreaming this function as a TimeVal method.
pub fn timeval_to_timespec(val: sys::time::TimeVal) -> Timespec {
    let usec = if val.tv_usec() > sys::time::suseconds_t::from(std::i32::MAX) {
        warn!("Cannot represent too-long usec quantity {} in timespec; using 0", val.tv_usec());
        0
    } else {
        val.tv_usec() as i32
    };
    Timespec::new(val.tv_sec() as sys::time::time_t, usec)
}

/// Converts a `sys::time::TimeVal` object into a `sys::time::TimeSpec`.
pub fn timeval_to_nix_timespec(val: sys::time::TimeVal) -> sys::time::TimeSpec {
    let usec = if val.tv_usec() > sys::time::suseconds_t::from(std::i32::MAX) {
        warn!("Cannot represent too-long usec quantity {} in timespec; using 0", val.tv_usec());
        0
    } else {
        val.tv_usec() as i64
    };
    sys::time::TimeSpec::nanoseconds((val.tv_sec() as i64) * 1_000_000_000 + usec)
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

/// Converts a set of `flags` bitmask to an `fs::OpenOptions`.
///
/// `allow_writes` indicates whether the file to be opened supports writes or not.  If the flags
/// don't match this condition, then this returns an error.
pub fn flags_to_openoptions(flags: u32, allow_writes: bool) -> NodeResult<fs::OpenOptions> {
    let flags = flags as i32;
    let oflag = fcntl::OFlag::from_bits_truncate(flags);

    let mut options = fs::OpenOptions::new();
    options.read(true);
    if oflag.contains(fcntl::OFlag::O_WRONLY) | oflag.contains(fcntl::OFlag::O_RDWR) {
        if !allow_writes {
            return Err(KernelError::from_errno(errno::Errno::EPERM));
        }
        if oflag.contains(fcntl::OFlag::O_WRONLY) {
            options.read(false);
        }
        options.write(true);
    }
    options.custom_flags(flags);
    Ok(options)
}

/// Asserts that two FUSE file attributes are equal.
//
// TODO(jmmv): Remove once rust-fuse 0.4 is released as it will derive Eq for FileAttr.
pub fn fileattrs_eq(attr1: &fuse::FileAttr, attr2: &fuse::FileAttr) -> bool {
    attr1.ino == attr2.ino
        && attr1.kind == attr2.kind
        && attr1.nlink == attr2.nlink
        && attr1.size == attr2.size
        && attr1.blocks == attr2.blocks
        && attr1.atime == attr2.atime
        && attr1.mtime == attr2.mtime
        && attr1.ctime == attr2.ctime
        && attr1.crtime == attr2.crtime
        && attr1.perm == attr2.perm
        && attr1.uid == attr2.uid
        && attr1.gid == attr2.gid
        && attr1.rdev == attr2.rdev
        && attr1.flags == attr2.flags
}

#[cfg(test)]
mod tests {
    use super::*;

    use nix::{errno, unistd};
    use nix::sys::time::TimeValLike;
    use std::fs::File;
    use std::io::{Read, Write};
    use std::os::unix;
    use std::time::Duration;
    use tempfile::tempdir;
    use testutils;

    /// Creates a file at `path` with the given `content` and closes it.
    fn create_file(path: &Path, content: &str) {
        let mut file = File::create(path).expect("Test file creation failed");
        let written = file.write(content.as_bytes()).expect("Test file data write failed");
        assert_eq!(content.len(), written, "Test file wasn't fully written");
    }

    #[test]
    fn test_timespec_to_timeval() {
        let spec = Timespec { sec: 123, nsec: 45000 };
        let val = timespec_to_timeval(spec);
        assert_eq!(123, val.tv_sec());
        assert_eq!(45, val.tv_usec());
    }

    #[test]
    fn test_timeval_to_timespec() {
        let val = sys::time::TimeVal::seconds(654) + sys::time::TimeVal::nanoseconds(123_456);
        let spec = timeval_to_timespec(val);
        assert_eq!(654, spec.sec);
        assert_eq!(123, spec.nsec);
    }

    #[test]
    fn test_timeval_to_nix_timespec() {
        let val = sys::time::TimeVal::seconds(654) + sys::time::TimeVal::nanoseconds(123_456);
        let spec = timeval_to_nix_timespec(val);
        assert_eq!(654, spec.tv_sec());
        assert_eq!(123, spec.tv_nsec());
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
        let dir = tempdir().unwrap();
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
        assert!(fileattrs_eq(&exp_attr, &attr));
    }

    #[test]
    fn test_attr_fs_to_fuse_regular() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("file");

        let content = "Some text\n";
        create_file(&path, content);

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
        assert!(fileattrs_eq(&exp_attr, &attr));
    }

    #[test]
    fn test_flags_to_openoptions_rdonly() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("file");
        create_file(&path, "original content");

        let flags = fcntl::OFlag::O_RDONLY.bits() as u32;
        let openoptions = flags_to_openoptions(flags, false).unwrap();
        let mut file = openoptions.open(&path).unwrap();

        write!(file, "foo").expect_err("Write to read-only file succeeded");

        let mut buf = String::new();
        file.read_to_string(&mut buf).expect("Read from read-only file failed");
        assert_eq!("original content", buf);
    }

    #[test]
    fn test_flags_to_openoptions_wronly() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("file");
        create_file(&path, "");

        let flags = fcntl::OFlag::O_WRONLY.bits() as u32;
        flags_to_openoptions(flags, false).expect_err("Writability permission not respected");
        let openoptions = flags_to_openoptions(flags, true).unwrap();
        let mut file = openoptions.open(&path).unwrap();

        let mut buf = String::new();
        file.read_to_string(&mut buf).expect_err("Read from write-only file succeeded");

        write!(file, "foo").expect("Write to write-only file failed");
    }

    #[test]
    fn test_flags_to_openoptions_rdwr() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("file");
        create_file(&path, "some content");

        let flags = fcntl::OFlag::O_RDWR.bits() as u32;
        flags_to_openoptions(flags, false).expect_err("Writability permission not respected");
        let openoptions = flags_to_openoptions(flags, true).unwrap();
        let mut file = openoptions.open(&path).unwrap();

        let mut buf = String::new();
        file.read_to_string(&mut buf).expect("Read from read/write file failed");

        write!(file, "foo").expect("Write to read/write file failed");
    }

    #[test]
    fn test_flags_to_openoptions_custom() {
        let dir = tempdir().unwrap();
        create_file(&dir.path().join("file"), "");
        let path = dir.path().join("link");
        unix::fs::symlink("file", &path).unwrap();

        {
            let flags = fcntl::OFlag::O_RDONLY.bits() as u32;
            let openoptions = flags_to_openoptions(flags, true).unwrap();
            openoptions.open(&path).expect("Failed to open symlink target; test setup bogus");
        }

        let flags = (fcntl::OFlag::O_RDONLY | fcntl::OFlag::O_NOFOLLOW).bits() as u32;
        let openoptions = flags_to_openoptions(flags, true).unwrap();
        openoptions.open(&path).expect_err("Open of symlink succeeded");
    }
}
