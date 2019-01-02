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
located in the [`man/sandboxfs.1`](man/sandboxfs.1) file.  You can view a
rendered version of this manual page using the following command after
cloning the tree:

    man ./man/sandboxfs.1

## Contributing

If you'd like to contribute to sandboxfs, there is plenty of work to be
done!  Please make sure to read our [contribution guidelines](CONTRIBUTING.md)
to learn about some important prerequisite steps.
