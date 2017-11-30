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
rootenv+=(UNPRIVILEGED_USER="${USER}")
[ "${GOPATH-unset}" = unset ] || rootenv+=(GOPATH="${GOPATH}")
[ "${GOROOT-unset}" = unset ] || rootenv+=(GOROOT="${GOROOT}")
readonly rootenv

do_bazel() {
  bazel run //admin/install -- --prefix="$(pwd)/local"

  # TODO(jmmv): We disable Bazel's sandboxing because it denies our tests from
  # using FUSE (e.g. accessing system-wide helper binaries).  Figure out a way
  # to not require this.
  bazel test --spawn_strategy=standalone --test_output=streamed //...
  sudo -H "${rootenv[@]}" SANDBOXFS="$(pwd)/local/bin/sandboxfs" -s \
      ./bazel-bin/integration/go_default_test -test.v -test.timeout=600s

  # Make sure we can install as root as documented in INSTALL.md.
  sudo ./bazel-bin/admin/install/install --prefix="$(pwd)/local-root"
}

do_gotools() {
  go build -o ./sandboxfs github.com/bazelbuild/sandboxfs/cmd/sandboxfs

  go test -v -timeout=600s github.com/bazelbuild/sandboxfs/internal/shell

  SANDBOXFS="$(pwd)/sandboxfs" SKIP_NOT_FOR_RELEASE_TEST=yes \
      go test -v -timeout=600s github.com/bazelbuild/sandboxfs/integration
  if SANDBOXFS="$(pwd)/sandboxfs" \
      go test -v -timeout=600s -run TestCli_VersionNotForRelease \
      github.com/bazelbuild/sandboxfs/integration; then
    echo "Tests did not catch that the current build is not for release" 1>&2
    exit 1
  else
    echo "Previous test was expected to fail, and it did! All good." 1>&2
  fi

  sudo -H "${rootenv[@]}" \
      SANDBOXFS="$(pwd)/sandboxfs" SKIP_NOT_FOR_RELEASE_TEST=yes -s \
      go test -v -timeout=600s github.com/bazelbuild/sandboxfs/integration
}

do_lint() {
  bazel run //admin/lint -- --verbose
}

case "${DO}" in
  bazel|gotools|lint)
    "do_${DO}"
    ;;

  *)
    echo "Unknown value for DO variable" 1>&2
    exit 1
    ;;
esac
