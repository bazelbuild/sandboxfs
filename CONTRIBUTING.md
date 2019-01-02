# Contributing

Want to contribute?  Great!  First, read this page in its entirety.

## Before you contribute

Before we can use your code, you must sign the [Google Individual Contributor
License Agreement](https://cla.developers.google.com/about/google-individual)
(CLA), which you can do online.

The CLA is necessary mainly because you own the copyright to your changes, even
after your contribution becomes part of our codebase, so we need your
permission to use and distribute your code.  We also need to be sure of various
other things: for instance that you'll tell us if you know that your code
infringes on other people's patents.  You don't have to sign the CLA until
after you've submitted your code for review and a member has approved it, but
you must do it before we can put your code into our codebase.

Contributions made by corporations are covered by a different agreement than
the one above, the [Software Grant and Corporate Contributor License
Agreement](https://cla.developers.google.com/about/google-corporate).

Before you start working on a larger contribution, you should get in touch with
us first through the issue tracker with your idea so that we can help out and
possibly guide you.  Coordinating up front makes it much easier to avoid
frustration later on.

## Project setup

In order to contribute to sandboxfs, you *must* use the Bazel build system, as
this integrates with a variety of tools that you will need during development.
Read the [installation instructions](INSTALL.md) for details on how to get
started.

Once you have Bazel installed, you *must* run the `./configure` script to
prepare your source tree for development.  In its simplest form, this will
install necessary Git hooks into the current workspace.  Failure to do so will
potentially result in commits that are not up to the standards that we expect
and delay your code reviews.

## IDE support

sandboxfs' build scripts integrate well with the [Visual Studio Code
(VSCode)](https://code.visualstudio.com/) editor.

To enable support for VSCode, run `./configure --enable-vscode`.  This will
generate a `settings.json` file for the project that points to the right
locations of the Go tools and will also create a fake `GOPATH` that the tools
can consume.

## Code formatting and linting

At commit time, our pre-commit script will verify that any changes you have
made comply with the expected coding style.  If there are any problems, you
will see a report on the command-line with the specifics.

If you want to run the style check by hand, you can invoke it with the
`make lint` command.

## Code reviews

All submissions, including submissions by project members, require review.
We use GitHub pull requests for this purpose.

Be aware that the copy of sandboxfs in GitHub is not *yet* primary: all
changes submitted via GitHub bugs or pull requests will be manually applied
to the Google-internal tree, reviewed there, and then reexported to GitHub
(which means your commit IDs will change, but attribution should not).  Our
goal is to make GitHub the primary copy, but we are not there yet!
