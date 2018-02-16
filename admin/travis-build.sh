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

do_bazel() {
  bazel run //admin/install -- --prefix="$(pwd)/local"

  # The globs below in the invocation of the binaries exist because of
  # https://github.com/bazelbuild/rules_go/issues/1239: we cannot predict
  # the path to the built binaries so we must discover it dynamically.
  # We know we have built them once, so these should only match one entry.

  # TODO(jmmv): We disable Bazel's sandboxing because it denies our tests from
  # using FUSE (e.g. accessing system-wide helper binaries).  Figure out a way
  # to not require this.
  bazel test --spawn_strategy=standalone --test_output=streamed //...
  sudo -H "${rootenv[@]}" -s \
      ./bazel-bin/integration/*/go_default_test -test.v -test.timeout=600s \
      -sandboxfs_binary="$(pwd)/local/bin/sandboxfs" \
      -unprivileged_user="${USER}"

  # Make sure we can install as root as documented in INSTALL.md.
  sudo ./bazel-bin/admin/install/*/install --prefix="$(pwd)/local-root"
}

do_gotools() {
  go build -o ./sandboxfs github.com/bazelbuild/sandboxfs/cmd/sandboxfs

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
