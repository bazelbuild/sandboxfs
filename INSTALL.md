# Installation instructions

Given that there have not yet been any formal releases of sandboxfs, there
currently are no prebuilt binaries available.  To use sandboxfs, you will
have to build and install it from a fresh checkout of the GitHub tree.

## From sources with Bazel

The preferred mechanism to build sandboxfs is to use the
[Bazel](http://bazel.build) build system.  The reason is that this provides
a reproducible build environment with pinned versions of all required
dependencies and also integrates well with various support tools used
during installation and during development.

To get started:

1.  [Download and install Bazel](https://bazel.build/).

1.  [Download and install Go](https://golang.org/).

    *   If you don't want to do this by hand and would rather have Bazel
        manage the installation of Go for you, edit the `WORKSPACE` file
        and remove the `go_version = "host"` parameter from the
        `go_register_toolchains` statement.

1.  Run `bazel build //cmd/sandboxfs` to download all required dependencies
    and build the final binary.

## From sources with the Go tools

You can also build and install sandboxfs from this Git repository using the
standard Go tools as follows:

    go get github.com/bazelbuild/sandboxfs/cmd/sandboxfs
    go build github.com/bazelbuild/sandboxfs/cmd/sandboxfs
