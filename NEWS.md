# Major changes between releases

## Changes in version 0.2.0

**STILL UNDER DEVELOPMENT; NOT RELEASED YET.**

* Issue #92: Fixed a bug in `readdir` where we would regenerate inodes for
  directory entries, causing later confusion when trying to access those directories.

* Issues #94, #98: Fixed a bug in `rename` that caused moved directories to
  lose access to their contents (because the underlying paths for their
  descendents wouldn't be updated to point to their new locations).

* Fixed a bug where writes on a file descriptor that had been duplicated and
  closed did not update the file size, resulting in bad data being returned
  on future reads.

* Fixed timestamp updates so that the `birthtime` rolls back to an older
  `mtime` to mimic BSD semantics.

* Fixed hardlink counts so that they are zero for handles that point to
  deleted files or directories.

* Added support for extended attributes.  Must be explicitly enabled by passing
  the `--xattrs` option.

* Added support to change the timestamps of a symlink on systems that have
  this feature.

* Disabled the path-based node cache by default and added a `--node_cache`
  flag to reenable it. This fixes crashes running Java within a sandboxfs
  instance where the Java toolchain is mapped under multiple locations and
  the first mapped location vanishes. See [The OSXFUSE, hard links, and dladdr
  puzzle](https://jmmv.dev/2020/01/osxfuse-hardlinks-dladdr.html) for details.

## Changes in version 0.1.1

**Released 2019-10-24.**

* Fixed the definition of `--input` and `--output` to require an argument,
  which makes `--foo bar` and `--foo=bar` equivalent.  This can be thought to
  break backwards compatibility but, in reality, it does not.  The previous
  behavior was just broken: specifying `--foo bar` would cause `bar` to be
  treated as an argument and `--foo` to use its default value, which meant
  that these two flags would be ignored when supplied under this syntax.

* Fixed `--input` and `--output` to handle stdin and stdout correctly when
  running e.g. under `sudo`.

* Make create operations honor the UID and GID of the caller user instead of
  inheriting the permissions of whoever was running sandboxfs.  Only has an
  effect when using `--allow=other` or `--allow=root`.

## Changes in version 0.1.0

**Released on 2019-02-05.**

This is the first formal release of the sandboxfs project.

**WARNING:** The interaction points with sandboxfs are subject to change at this
point.  In particular, the command-line interface and the data format used to
reconfigure sandboxfs while it's running *will* most certainly change.
