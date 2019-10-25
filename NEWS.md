# Major changes between releases

## Changes in version 0.2.0

**STILL UNDER DEVELOPMENT; NOT RELEASED YET.**

*   Changed the reconfiguration protocol to use JSON streams for both the
    requests and the responses, instead of the previous ad-hoc line-oriented
    protocol.

*   Changed the reconfiguration protocol so that each map and unmap request
    carries a list of mappings to map and unmap, respectively, along with the
    "root" path where all those mappings start.  This is to allow sandboxfs to
    process the requests more efficiently.

*   Changed the reconfiguration protocol so that each request contains a tag,
    which is then propagated to the response for that request.  This is to
    allow sandboxfs to process requests in parallel.

*   Changed the reconfiguration protocol to work at the level of sandboxes,
    not paths, where a sandbox is defined as a top-level directory with a
    collection of mappings beneath it.

    This essentially makes reconfigurations less powerful than they were, but
    also makes them infinitely simpler to understand and manage.  Furthermore,
    this lines up better with the needs of Bazel, our primary customer, and
    with sandboxfs' own name.

*   Fixed a bug where writes on a file descriptor that had been duplicated and
    closed did not update the file size, resulting in bad data being returned
    on future reads.

*   Fixed timestamp updates so that the `birthtime` rolls back to an older
    `mtime` to mimic BSD semantics.

*   Fixed hardlink counts so that they are zero for handles that point to
    deleted files or directories.

*   Added support for extended attributes.  Must be explicitly enabled by
    passing the `--xattrs` option.

*   Added support to change the timestamps of a symlink on systems that have
    this feature.

*   Disabled the path-based node cache by default and added a `--node_cache`
    flag to reenable it.  This fixes crashes running Java within a sandboxfs
    instance where the Java toolchain is mapped under multiple locations and
    the first mapped location vanishes.  See [The OSXFUSE, hard links, and
    dladdr puzzle](https://jmmv.dev/2020/01/osxfuse-hardlinks-dladdr.html) for
    details.

## Changes in version 0.1.1

**Released 2019-10-24.**

*   Fixed the definition of `--input` and `--output` to require an argument,
    which makes `--foo bar` and `--foo=bar` equivalent.  This can be thought to
    break backwards compatibility but, in reality, it does not.  The previous
    behavior was just broken: specifying `--foo bar` would cause `bar` to be
    treated as an argument and `--foo` to use its default value, which meant
    that these two flags would be ignored when supplied under this syntax.

*   Fixed `--input` and `--output` to handle stdin and stdout correctly when
    running e.g. under `sudo`.

*   Make create operations honor the UID and GID of the caller user instead of
    inheriting the permissions of whoever was running sandboxfs.  Only has an
    effect when using `--allow=other` or `--allow=root`.

## Changes in version 0.1.0

**Released on 2019-02-05.**

This is the first formal release of the sandboxfs project.

**WARNING:** The interaction points with sandboxfs are subject to change at this
point.  In particular, the command-line interface and the data format used to
reconfigure sandboxfs while it's running *will* most certainly change.
