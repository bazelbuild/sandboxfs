# sandboxfs: A virtual file system for sandboxing

sandboxfs is a **FUSE file system** that exposes a combination of multiple
files and directories from the host's file system in the form of a virtual
tree with an arbitrary layout.  You can think of a sandbox as an arbitrary
**view into the host's file system** with different access privileges per
directory.

sandboxfs is **designed to allow running commands** with limited access to
the file system by using the virtual tree as their new root, and to do so
consistently across a variety of platforms.

sandboxfs is **licensed under the [Apache 2.0 license](LICENSE)** and is
not an official Google product.

## Releases

sandboxfs is still under active development and there have not yet been any
formal releases.

See the [installation instructions](INSTALL.md) for details on how to build
and install sandboxfs.

See the [release notes](NEWS.md) file for more details.

## Usage

sandboxfs is fully documented in the `sandboxfs(1)` manual page, which is
located in the [`cmd/sandboxfs/sandboxfs.1`](cmd/sandboxfs/sandboxfs.1)
file.  You can view a rendered version of this manual page using the
following command after cloning the tree:

    man ./cmd/sandboxfs/sandboxfs.1

## Go and Rust

The current implementation of sandboxfs (the one you get when you follow
the installation instructions) is written in Go.  This is known to work
well but has some performance problems caused by the way the Go FUSE
bindings work (see e.g. https://github.com/bazil/fuse/issues/129).

A prototype rewrite in Rust showed the potential of obtaining much better
performance and also highlighted multiple concurrency issues in the
current Go implementation.  As a result, a decision was made to reimplement
sandboxfs in Rust for safety and performance reasons.  The `src`
subdirectory contains the still-partial reimplementation, which will be a
drop-in replacement for the Go one once complete.

## Contributing

If you'd like to contribute to sandboxfs, there is plenty of work to be
done!  Please make sure to read our [contribution guidelines](CONTRIBUTING.md)
to learn about some important prerequisite steps.
