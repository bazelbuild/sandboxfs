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

do_lint() {
  bazel run //admin/lint -- --verbose
  PATH="${HOME}/.cargo/bin:${PATH}" cargo clippy -- -D warnings
}

do_rust() {
  PATH="${HOME}/.cargo/bin:${PATH}"

  # The Rust implementation of sandboxfs is still experimental and is unable to
  # pass all integration tests.  This blacklist keeps track of which those are.
  # Ideally, by the time this alternative implementation is ready, this list
  # will be empty and we can then refactor this file to just test one version.
  local blacklist=(
    TestDebug_FuseOpsInLog
    TestProfiling_Http
    TestProfiling_FileProfiles
    TestProfiling_BadConfiguration
    TestReconfiguration_Streams
    TestReconfiguration_Steps
    TestReconfiguration_Unmap
    TestReconfiguration_RemapInvalidatesCache
    TestReconfiguration_Errors
    TestReconfiguration_RaceSystemComponents
    TestReconfiguration_DirectoryListings
    TestReconfiguration_InodesAreStableForSameUnderlyingFiles
    TestReconfiguration_WritableNodesAreDifferent
    TestReconfiguration_FileSystemStillWorksAfterInputEOF
    TestReconfiguration_StreamFileDoesNotExist
  )

  # TODO(https://github.com/bazelbuild/rules_rust/issues/2): Replace by a
  # Bazel-based build once the Rust rules are capable of doing so.
  cargo build
  local bin="$(pwd)/target/debug/sandboxfs"
  cargo test --verbose

  local all=(
      $(go test -test.list=".*" github.com/bazelbuild/sandboxfs/integration \
          -sandboxfs_binary=irrelevant -rust_variant=true | grep -v "^ok")
  )

  # Compute the list of tests to run by comparing the full list of tests that
  # we got from the test program and taking out all blacklisted tests.  This is
  # O(n^2), sure, but it doesn't matter: the list is small and this will go away
  # at some point.
  set +x
  local valid=()
  for t in "${all[@]}"; do
    local blacklisted=no
    for o in "${blacklist[@]}"; do
      if [ "${t}" = "${o}" ]; then
        blacklisted=yes
        break
      fi
    done
    if [ "${blacklisted}" = yes ]; then
      echo "Skipping blacklisted test ${t}" 1>&2
      continue
    fi
    valid+="${t}"
  done
  set -x

  [ "${#valid[@]}" -gt 0 ] || return 0  # Only run tests if any are valid.
  for t in "${valid[@]}"; do
    go test -v -timeout=600s -test.run="^${t}$" \
        github.com/bazelbuild/sandboxfs/integration \
        -sandboxfs_binary="${bin}" -release_build=false

    sudo -H "${rootenv[@]}" -s \
        go test -v -timeout=600s -test.run="^${t}$" \
        github.com/bazelbuild/sandboxfs/integration \
        -sandboxfs_binary="$(pwd)/sandboxfs" -release_build=false \
        -rust_variant=true
  done
}

case "${DO}" in
  bazel|gotools|lint|rust)
    "do_${DO}"
    ;;

  *)
    echo "Unknown value for DO variable" 1>&2
    exit 1
    ;;
esac
