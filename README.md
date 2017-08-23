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

See the [release notes](NEWS.md) file for more details.

## Usage

sandboxfs is fully documented in the `sandboxfs(1)` manual page, which is
located in the [`cmd/sandboxfs/sandboxfs.1`](cmd/sandboxfs/sandboxfs.1)
file.  You can view a rendered version of this manual page using the
following command after cloning the tree:

    man ./cmd/sandboxfs/sandboxfs.1

## Contributing

If you'd like to contribute to sandboxfs, there is plenty of work to be
done!  As a start, you can look into all the `TODO`s sprinkled throughout
the codebase.  But before you dive into contributing, please make sure to
read our [contribution guidelines](CONTRIBUTING.md).
