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

do_install() {
  ./configure --cargo="${HOME}/.cargo/bin/cargo" --goroot=none \
      --prefix="/opt/sandboxfs"
  make release
  make install DESTDIR="$(pwd)/destdir"
  test -x destdir/opt/sandboxfs/bin/sandboxfs
  test -e destdir/opt/sandboxfs/share/man/man1/sandboxfs.1
  test -e destdir/opt/sandboxfs/share/doc/sandboxfs/README.md
}

do_lint() {
  ./configure --cargo="${HOME}/.cargo/bin/cargo"
  make lint
}

do_test() {
  ./configure --cargo="${HOME}/.cargo/bin/cargo"
  make debug
  make check
  sudo -H "${rootenv[@]}" -s make check-integration \
      CHECK_INTEGRATION_FLAGS=-unprivileged_user="${USER}"
}

case "${DO}" in
  install|lint|test)
    "do_${DO}"
    ;;

  *)
    echo "Unknown value for DO variable" 1>&2
    exit 1
    ;;
esac
