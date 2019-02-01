#! /bin/bash
# Copyright 2017 Google Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License"); you may not
# use this file except in compliance with the License.  You may obtain a copy
# of the License at:
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
# WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.  See the
# License for the specific language governing permissions and limitations
# under the License.

set -e -u -x

# Default to no features to avoid cluttering .travis.yml.
: "${FEATURES:=}"

rootenv=()
rootenv+=(PATH="${PATH}")
[ "${GOPATH-unset}" = unset ] || rootenv+=(GOPATH="${GOPATH}")
[ "${GOROOT-unset}" = unset ] || rootenv+=(GOROOT="${GOROOT}")
readonly rootenv

# Verifies that Bazel (our primary customer) integrates well with sandboxfs.
# This is a simple smoke test that builds Bazel with itself: there is no
# guarantee that more complex builds wouldn't fail due to sandboxfs bugs.
do_bazel() {
  ./configure --cargo="${HOME}/.cargo/bin/cargo" --features="${FEATURES}" \
      --goroot=none
  make release
  ( cd bazel && bazel \
      build \
      --experimental_use_sandboxfs \
      --experimental_sandboxfs_path="$(pwd)/../target/release/sandboxfs" \
      --spawn_strategy=sandboxed \
      //src:bazel )
  ./bazel/bazel-bin/src/bazel help
}

# Verifies that the "make install" procedure works and respects both the
# user-supplied prefix and destdir.
do_install() {
  ./configure --cargo="${HOME}/.cargo/bin/cargo" --features="${FEATURES}" \
      --goroot=none --prefix="/opt/sandboxfs"
  make release
  make install DESTDIR="$(pwd)/destdir"
  test -x destdir/opt/sandboxfs/bin/sandboxfs
  test -e destdir/opt/sandboxfs/share/man/man1/sandboxfs.1
  test -e destdir/opt/sandboxfs/share/doc/sandboxfs/README.md
}

# Ensures that the source tree is sane according to our coding style.
do_lint() {
  ./configure --cargo="${HOME}/.cargo/bin/cargo" --features="${FEATURES}"
  make lint
}

# Ensures that we can build a publishable crate and that it is sane.
do_package() {
  # Intentionally avoids ./configure to certify that the code is buildable
  # directly from Cargo.
  "${HOME}/.cargo/bin/cargo" publish --dry-run
}

# Runs sandboxfs' unit and integration tests.
do_test() {
  ./configure --cargo="${HOME}/.cargo/bin/cargo" --features="${FEATURES}"
  make debug
  make check
  sudo -H "${rootenv[@]}" -s make check-integration \
      CHECK_INTEGRATION_FLAGS=-unprivileged_user="${USER}"
}

case "${DO}" in
  bazel|install|lint|package|test)
    "do_${DO}"
    ;;

  *)
    echo "Unknown value for DO variable" 1>&2
    exit 1
    ;;
esac
