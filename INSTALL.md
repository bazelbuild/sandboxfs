# Installation instructions

## Using the macOS installer

1.  [Download and install OSXFUSE](https://osxfuse.github.io/).

1.  Download the `sandboxfs-<release>-<date>.pkg` file attached to the
    latest release in the
    [releases page](https://github.com/bazelbuild/sandboxfs/releases).

1.  Double-click the downloaded file and follow the instructions.

Should you want to uninstall sandboxfs at any point, you can run
`/usr/local/share/sandboxfs/uninstall.sh` to cleanly remove all installed
files.

## From crates.io

1.  [Download and install Rust](https://www.rust-lang.org/).  If you already
    had it installed, make sure you are on a new-enough version by running
    `rustup update`.

1.  Download and install FUSE for your system.  On Linux this will vary
    on a distribution basis, and on macOS you can [install
    OSXFUSE](https://osxfuse.github.io/).

1.  Run `cargo install sandboxfs`.

## From a GitHub checkout

1.  [Download and install Rust](https://www.rust-lang.org/).  If you already
    had it installed, make sure you are on a new-enough version by running
    `rustup update`.

1.  Download and install FUSE for your system.  On Linux this will vary
    on a distribution basis, and on macOS you can [install
    OSXFUSE](https://osxfuse.github.io/).

1.  Download and install `pkg-config` or `pkgconf`.

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

## Profiling support

sandboxfs has optional support for the gperftools profiling tools.  If you have
that package installed, you can pass `--features=profiling` to the `configure`
script and sandboxfs's `--cpu_profile` flag will become functional.
