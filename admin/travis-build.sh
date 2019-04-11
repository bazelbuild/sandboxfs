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

# Builds the Linux binary distribution and verifies that it works.
do_linux_pkg() {
  ./admin/make-linux-pkg.sh --cargo="${HOME}/.cargo/bin/cargo"
  local pkg="$(echo sandboxfs-*.*.*-????????-linux-*.tgz)"

  sudo find /usr/local >before.list
  sudo tar xzv -C /usr/local -f "${pkg}"
  /usr/local/bin/sandboxfs --version

  sudo /usr/local/libexec/sandboxfs/uninstall.sh
  sudo find /usr/local >after.list
  if ! cmp -s before.list after.list; then
    echo "Files left behind after installation:"
    diff -u before.list after.list
    false
  fi
}

# Builds the macOS installer and verifies that it works.
do_macos_pkg() {
  ./admin/make-macos-pkg.sh --cargo="${HOME}/.cargo/bin/cargo"
  local pkg="$(echo sandboxfs-*.*.*-????????-macos.pkg)"

  sudo find /Library /etc /usr/local >before.list
  sudo sysctl -w vfs.generic.osxfuse.tunables.allow_other=0

  sudo installer -pkg "${pkg}" -target /
  test "$(sysctl -n vfs.generic.osxfuse.tunables.allow_other)" -eq 1
  /usr/local/bin/sandboxfs --version

  sudo /usr/local/libexec/sandboxfs/uninstall.sh
  sudo find /Library /etc /usr/local >after.list
  if ! cmp -s before.list after.list; then
    echo "Files left behind after installation:"
    diff -u before.list after.list
    false
  fi
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

  # Now that we have built all of our unit tests (via "make check"), find
  # where those are and rerun them as root.  Note that we cannot simply
  # call cargo as sudo because it won't work with the user-specific
  # installation we performed.
  local tests="$(find target/debug -maxdepth 1 -type f -name sandboxfs-*
      -perm -0100)"
  if [ -z "${tests}" ]; then
    echo "Cannot find already-built unit tests"
    find target
    false
  fi
  for t in ${tests}; do
    sudo -H "${rootenv[@]}" UNPRIVILEGED_USER="${USER}" "${t}"
  done

  sudo -H "${rootenv[@]}" -s make check-integration \
      CHECK_INTEGRATION_FLAGS=-unprivileged_user="${USER}"
}

case "${DO}" in
  bazel|install|lint|linux_pkg|macos_pkg|package|test)
    "do_${DO}"
    ;;

  *)
    echo "Unknown value for DO variable" 1>&2
    exit 1
    ;;
esac
