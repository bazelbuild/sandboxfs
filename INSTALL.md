# Installation instructions

Given that there have not yet been any formal releases of sandboxfs, there
currently are no prebuilt binaries available.  To use sandboxfs, you will
have to build and install it from a fresh checkout of the GitHub tree.

1.  [Download and install Rust](https://www.rust-lang.org/).

1.  Run `./configure` to generate the scripts that will allow installation.

    1.  The default installation path is `/usr/local` but you can customize
        it to any other location by passing the flag `--prefix=/usr/local`.

    1.  The build scripts will use `cargo` from your path but you can set
        a different location by passing the `--cargo=/path/to/cargo` flag.

    1.  The build scripts will use `go` from your path but you can set a
        different location by passing the `--goroot=/path/to/goroot` flag.
        You can also use the magic `none` value to disable Go usage, but
        this will prevent running the integration tests or the code linter.

1.  Run `make release` to download all required dependencies and build the
    final binary.

    1.  You could also run `cargo build --release`.  This package is
        intended to work just fine with the standard Cargo toolchain.

1.  Run `make install` to put the built binary and all supporting files
    in place.

    *   You will (most likely) need superuser permissions to install
        under `/usr/local`, so run the previous command with `sudo`.
