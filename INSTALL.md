# Installation instructions

## Using the macOS installer

1.  [Download and install OSXFUSE](https://osxfuse.github.io/).

1.  Download the `sandboxfs-<release>-<date>-macos.pkg` file attached to the
    latest release in the
    [releases page](https://github.com/bazelbuild/sandboxfs/releases).

1.  Double-click the downloaded file and follow the instructions.

Should you want to uninstall sandboxfs at any point, you can run
`/usr/local/share/sandboxfs/uninstall.sh` to cleanly remove all installed
files.

## Using the generic Linux pre-built binaries

If your Linux distribution does not provide sandboxfs packages on its own,
you can try using the prebuilt versions we supply.  These assume that you
have certain versions of shared libraries in the right location, so your
mileage may vary when attempting to use these.  If the binaries do not
work, you'll have to build sandboxfs from the sources as described later on.

1.  Install FUSE.  The specific steps will depend on your distribution but
    here are some common examples:

    *   Debian, Ubuntu: `apt install libfuse2`.
    *   Fedora: `dnf install fuse-libs`.

1.  Download the `sandboxfs-<release>-<date>-linux-<arch>.tgz` file attached
    to the latest release in the
    [releases page](https://github.com/bazelbuild/sandboxfs/releases).

1.  Extract the downloaded file in the prefix where you want sandboxfs to
    be available.  The most common location for a system-wide installation
    is `/usr/local` so do:

    ```
    tar xzv -C /usr/local -f .../path/to/downloaded.tgz
    ```

    You can also install sandboxfs under your home directory if you do not
    have administrator privileges, for example under `~/local`.

1.  Verify that the binary can start by running it at least once.  If it
    does start (because it can find all required shared libraries), you can
    be fairly confident that it will work.

Should you want to uninstall sandboxfs at any point, you can run
`.../share/sandboxfs/uninstall.sh` from the prefix in which you unpacked the
pre-built binaries to cleanly remove all installed files.

## From crates.io

1.  [Download and install Rust](https://www.rust-lang.org/).  If you already
    had it installed, make sure you are on a new-enough version by running
    `rustup update`.

1.  Install FUSE and pkg-config.  The specific steps will depend on your
    system but here are some common examples:

    *   Debian, Ubuntu: `apt install libfuse-dev pkg-config`.
    *   Fedora: `dnf install fuse-devel pkgconf-pkg-config`.
    *   macOS: Visit the [OSXFUSE](https://osxfuse.github.io/) homepage and
        try `brew install pkg-config`.

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

## macOS only: Enable "allow other" support in OSXFUSE

In order to run system binaries within a sandboxfs mount point (which is
the primary goal of using sandboxfs), you must enable OSXFUSE's "allow
other" support; otherwise, necessary core macOS security services [will deny
executions](http://julio.meroh.net/2017/10/fighting-execs-sandboxfs-macos.html).

To do this, run the following for a one-time change:

    sudo sysctl -w vfs.generic.osxfuse.tunables.allow_other=1

Note that you cannot do this via `/etc/sysctl.conf` because the OSXFUSE
kernel extension has not yet been loaded when this file is parsed.

The macOS installer configures your system to do this automatically at
every boot by using a launch agent.
