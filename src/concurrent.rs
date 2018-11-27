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

use failure::Error;
use nix::unistd;
use nix::sys::signal;
use signal_hook;
use std::cmp;
use std::fs;
use std::io::{self, Read};
use std::os::unix::io as unix_io;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc;
use std::time;
use std::thread;

/// A file with a single scoped owner but with multiple non-owner views.
///
/// A `ShareableFile` object owns the file passed to it at construction time and will close the
/// underlying file handle when the object is dropped.
///
/// Concurrent views into this same file, obtained via the `clone_unowned` method, do not own the
/// file handle.  Such views must accept the fact that the handle can be closed at any time, a
/// condition that is simply exposed as if the handle reached EOF.  Concurrent views can safely be
/// moved across threads.
#[allow(unused)]  // TODO(jmmv): Remove once we use this code for reconfigurations.
pub struct ShareableFile {
    /// Underlying file descriptor shared across all views of this file.
    fd: unix_io::RawFd,

    /// Whether this instance of the file is responsible for closing the file descriptor.
    owned: bool,

    /// Whether the file descriptor has already been closed or not.
    ///
    /// In principle, we don't need this field: concurrent readers of this file will get their
    /// reads abruptly terminated with an error, and we should be able to rely on EBADFD to tell
    /// that this happened because of us closing the file descriptor.  Unfortunately... macOS
    /// Mojave doesn't cooperate and yields very strange error codes (like EISDIR) pretty frequently
    /// (but not deterministically).  May it be an APFS bug?
    closed: Arc<AtomicBool>,
}

impl ShareableFile {
    /// Constructs a new `ShareableFile` from an open file and takes ownership of it.
    #[allow(unused)]  // TODO(jmmv): Remove once we use this code for reconfigurations.
    pub fn from(file: fs::File) -> ShareableFile {
        use std::os::unix::io::IntoRawFd;
        ShareableFile {
            fd: file.into_raw_fd(),
            owned: true,
            closed: Arc::from(AtomicBool::new(false)),
        }
    }

    /// Returns an unowned view of the file.
    ///
    /// Users of this file must accept that the file can be closed at any time by the owner.
    #[allow(unused)]  // TODO(jmmv): Remove once we use this code for reconfigurations.
    pub fn clone_unowned(&mut self) -> ShareableFile {
        ShareableFile {
            fd: self.fd,
            owned: false,
            closed: self.closed.clone(),
        }
    }
}

impl Drop for ShareableFile {
    fn drop(&mut self) -> () {
        if self.owned {
            debug!("Closing ShareableFile with fd {}", self.fd);

            // We must mark the file as closed before attempting to close it because, when we do
            // the actual close, any threads blocked on a read will get unlocked immediately and
            // we won't have a chance to update the status.  It doesn't matter if the close succeeds
            // or not though, because we cannot do anything useful in the latter case anyway.
            self.closed.store(true, Ordering::SeqCst);

            if let Err(e) = unistd::close(self.fd) {
                warn!("Failed to close fd {}: {}", self.fd, e);
            }
        }
    }
}

impl Read for ShareableFile {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        match unistd::read(self.fd, buf) {
            Ok(n) => Ok(n),
            Err(nix::Error::Sys(errno)) => {
                if self.closed.load(Ordering::SeqCst) {
                    Ok(0)  // Simulate EOF due to close in another thread.
                } else {
                    Err(io::Error::from_raw_os_error(errno as i32))
                }
            },
            Err(e) => panic!(
                "Did not expect to get an error without an errno from a nix call: {:?}", e),
        }
    }
}

/// List of termination signals that cause the mount point to be correctly unmounted.
static CAPTURED_SIGNALS: [signal::Signal; 4] = [
    signal::Signal::SIGHUP,
    signal::Signal::SIGTERM,
    signal::Signal::SIGINT,
    signal::Signal::SIGQUIT,
];

/// Two-phase installer for `SignalsHandler`, which is responsible for unmounting a file system.
///
/// Installing the signals is tricky business because of a potential race: if the signal handler is
/// installed and a signal arrives *before* the mount point has been configured, the unmounting will
/// not succeed, which means we will enter the server loop and lose the signal.  Conversely, if we
/// did this backwards, we could receive a signal after the mount point has been configured but
/// before we install the signal handler, which means we'd terminate but leak the mount point.
///
/// To solve this, we must block signals while the mount point is being set up.  We achieve this by
/// exposing an interface that forces the caller to take two steps before it can obtain the actual
/// `SignalsHandler` object: the caller must call `prepare()` before mounting the file system and
/// then call `install()` once the file system is ready to serve.
///
/// Keeping this logic as a separate `SignalsInstaller` object, instead of trying to expose a "safe
/// mount function" helps ensure restoration of the original signal mask in all cases because this
/// type does so at drop time.
pub struct SignalsInstaller {
    /// Signal mask to restore at drop time.
    old_sigset: signal::SigSet,
}

impl SignalsInstaller {
    /// Blocks signals in preparation to mount the file system.
    pub fn prepare() -> SignalsInstaller {
        let mut old_sigset = signal::SigSet::empty();
        let mut sigset = signal::SigSet::empty();
        for signal in CAPTURED_SIGNALS.iter() {
            sigset.add(*signal);
        }
        signal::pthread_sigmask(
            signal::SigmaskHow::SIG_BLOCK, Some(&sigset), Some(&mut old_sigset))
            .expect("pthread_sigmask is not expected to fail");
        SignalsInstaller { old_sigset }
    }

    /// Installs all signal handlers to unmount the given `mount_point`.
    pub fn install(self, mount_point: PathBuf) -> Result<SignalsHandler, Error> {
        let (signal_sender, signal_receiver) = mpsc::channel();

        let mut signums = vec!();
        for signal in CAPTURED_SIGNALS.iter() {
            signums.push(*signal as i32);
        }
        let signals = signal_hook::iterator::Signals::new(&signums)?;

        std::thread::spawn(move || SignalsHandler::handler(&signals, mount_point, &signal_sender));

        Ok(SignalsHandler { signal_receiver })

        // Consumes self which causes the original signal mask to be restored and thus unblocks
        // signals.
    }
}

impl Drop for SignalsInstaller {
    fn drop(&mut self) -> () {
        signal::pthread_sigmask(signal::SigmaskHow::SIG_SETMASK, Some(&self.old_sigset), None)
            .expect("pthread_sigmask is not expected to fail and we cannot correctly clean up");
    }
}

/// Tries to unmount the given file system indefinitely.
///
/// If unmounting fails, it is probably because the file system is busy.  We don't know but it
/// doesn't matter: we have entered a terminal status: we do this at exit time so we'll keep trying
/// to unclog things while telling the user what's going on.  They are the ones that have to fix
/// this situation.
fn retry_unmount<P: AsRef<Path>>(mount_point: P) {
    let mut backoff = time::Duration::from_millis(10);
    let goal = time::Duration::from_secs(1);
    'retry: loop {
        match fuse::unmount(mount_point.as_ref()) {
            Ok(()) => break 'retry,
            Err(e) => {
                if backoff >= goal {
                    warn!("Unmounting file system failed with error '{}'; will retry in {:?}",
                        e, backoff);
                }
                thread::sleep(backoff);
                if backoff < goal {
                    backoff = cmp::min(goal, backoff * 2);
                }
            },
        }
    }
}

/// Maintains state and allows interaction with the installed signal handler.
///
/// The signal handler is responsible for unmounting the file system, which in turn causes the
/// FUSE serve loop to either never start or to finish execution if it was already running.
pub struct SignalsHandler {
    /// Channel used to receive, on the main thread, the number of the signal that was captured.
    // TODO(https://github.com/vorner/signal-hook/pull/8): Replace i32 with SigNo once merged.
    signal_receiver: mpsc::Receiver<i32>,
}

impl SignalsHandler {
    /// Returns the signal that was caught, if any.
    ///
    /// This is *not* racy when used after the file system has been unmounted (i.e. once the FUSE
    /// loop terminates).
    pub fn caught(&self) -> Option<i32> {
        match self.signal_receiver.try_recv() {
            Ok(signo) => Some(signo),
            Err(_) => None,
        }
    }

    /// The signal handler.
    ///
    /// This blocks until the receipt of the first signal and then ignores the rest.
    ///
    /// Upon receipt of a signal from `signals`, the handler first updates `signal_sender` with the
    /// number of the received signal and then attempts to unmount `mount_point` indefinitely to
    /// unblock the main FUSE loop.
    fn handler(signals: &signal_hook::iterator::Signals, mount_point: PathBuf,
        signal_sender: &mpsc::Sender<i32>) -> () {
        let signo = signals.forever().next().unwrap();
        if let Err(e) = signal_sender.send(signo) {
            warn!("Failed to propagate signal to main thread; will get stuck exiting: {}", e);
        }
        info!("Caught signal {}; unmounting {}", signo, mount_point.display());
        retry_unmount(mount_point);

        // It'd be nice if we could just "drop(signals)" here and then send the same received signal
        // to ourselves so that the program terminated with the correct exit status.  Unfortunately,
        // "drop(signals)" unregisters our hooks from the signal handlers installed by signal-hook
        // but it does not actually return the signal handlers to their original values.  This is a
        // limitation of the signal-hook crate.  Instead, we need the "caught" hack above to let the
        // main thread return an error code instead.
    }
}

#[cfg(test)]
mod tests {
    use nix::sys;
    use std::io::{Read, Write};
    use std::path::Path;
    use std::thread;
    use super::*;
    use tempfile;

    #[test]
    fn test_shareable_file_clones_share_descriptor_and_only_one_owns() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("file");
        fs::File::create(&path).unwrap().write_all(b"ABCDEFG").unwrap();

        fn read_one_byte(input: &mut impl Read) -> u8 {
            let mut buffer = [0];
            input.read_exact(&mut buffer).unwrap();
            buffer[0]
        }

        let mut file = ShareableFile::from(fs::File::open(&path).unwrap());
        let mut reader1 = file.clone_unowned();
        let mut reader2 = file.clone_unowned();
        assert_eq!(b'A', read_one_byte(&mut reader1));
        drop(reader1);  // Make sure dropping a non-owner copy doesn't close the file handle.
        assert_eq!(b'B', read_one_byte(&mut file));
        assert_eq!(b'C', read_one_byte(&mut reader2));
        drop(file);  // Closes the file descriptor so the readers should now not be able to read.
        let mut buffer = [0];
        assert_eq!(0, reader2.read(&mut buffer).expect("Expected 0 byte count as EOF after close"));
    }

    #[test]
    fn test_shareable_file_close_unblocks_reads_without_error() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("pipe");
        unistd::mkfifo(&path, sys::stat::Mode::S_IRUSR | sys::stat::Mode::S_IWUSR).unwrap();

        // We need to write to the FIFO for the read-only open below to not block.
        let writer_handle = {
            let path = path.clone();
            thread::spawn(move || {
                let mut writer = fs::File::create(&path).unwrap();
                write!(writer, "some text longer than we'll ever read")
            })
        };

        let mut file = ShareableFile::from(fs::File::open(&path).unwrap());

        let reader_handle = {
            let mut file = file.clone_unowned();
            thread::spawn(move || {
                let mut buffer = [0];
                file.read(&mut buffer)
            })
        };

        writer_handle.join().unwrap().expect("Write didn't finish successfully");
        drop(file);  // This should unblock the reader thread and let the join complete.
        reader_handle.join().unwrap().expect("Read didn't return success on EOF-like condition");
    }
}
