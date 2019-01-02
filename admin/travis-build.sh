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

rootenv=()
rootenv+=(PATH="${PATH}")
[ "${GOPATH-unset}" = unset ] || rootenv+=(GOPATH="${GOPATH}")
[ "${GOROOT-unset}" = unset ] || rootenv+=(GOROOT="${GOROOT}")
readonly rootenv

do_gotools() {
  go build -o ./sandboxfs github.com/bazelbuild/sandboxfs/cmd/sandboxfs

  go test -v -timeout=600s github.com/bazelbuild/sandboxfs/internal/sandbox
  go test -v -timeout=600s github.com/bazelbuild/sandboxfs/internal/shell

  go test -v -timeout=600s github.com/bazelbuild/sandboxfs/integration \
      -sandboxfs_binary="$(pwd)/sandboxfs" -release_build=false
  if go test -v -timeout=600s -run TestCli_VersionNotForRelease \
      github.com/bazelbuild/sandboxfs/integration \
      -sandboxfs_binary="$(pwd)/sandboxfs"
  then
    echo "Tests did not catch that the current build is not for release" 1>&2
    exit 1
  else
    echo "Previous test was expected to fail, and it did! All good." 1>&2
  fi

  sudo -H "${rootenv[@]}" -s \
      go test -v -timeout=600s github.com/bazelbuild/sandboxfs/integration \
      -sandboxfs_binary="$(pwd)/sandboxfs" -release_build=false
}

do_install() {
  ./configure --cargo="${HOME}/.cargo/bin/cargo" --prefix="/opt/sandboxfs"
  make release
  make install DESTDIR="$(pwd)/destdir"
  test -x destdir/opt/sandboxfs/bin/sandboxfs
  test -e destdir/opt/sandboxfs/share/man/man1/sandboxfs.1
  test -e destdir/opt/sandboxfs/share/doc/sandboxfs/README.md
}

do_lint() {
  bazel run //admin/lint -- --verbose
  PATH="${HOME}/.cargo/bin:${PATH}" cargo clippy -- -D warnings
}

do_rust() {
  PATH="${HOME}/.cargo/bin:${PATH}"

  # TODO(https://github.com/bazelbuild/rules_rust/issues/2): Replace by a
  # Bazel-based build once the Rust rules are capable of doing so.
  cargo build
  local bin="$(pwd)/target/debug/sandboxfs"
  cargo test --verbose

  go test -v -timeout=600s \
      github.com/bazelbuild/sandboxfs/integration \
      -sandboxfs_binary="${bin}" -release_build=false \
      -rust_variant=true

  sudo -H "${rootenv[@]}" -s \
      go test -v -timeout=600s \
      github.com/bazelbuild/sandboxfs/integration \
      -sandboxfs_binary="${bin}" -release_build=false \
      -rust_variant=true -unprivileged_user="${USER}"
}

case "${DO}" in
  gotools|install|lint|rust)
    "do_${DO}"
    ;;

  *)
    echo "Unknown value for DO variable" 1>&2
    exit 1
    ;;
esac
