#! /bin/sh
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

set -e -u

# Default to no features to avoid cluttering .travis.yml.
: "${FEATURES:=}"

install_bazel() {
  local osname
  case "${TRAVIS_OS_NAME}" in
    osx) osname=darwin ;;
    *) osname="${TRAVIS_OS_NAME}" ;;
  esac

  local tag=0.21.0  # Keep version in sync with travis-build.sh.
  local github="https://github.com/bazelbuild/bazel/releases/download/${tag}"
  local url="${github}/bazel-${tag}-${osname}-x86_64"
  mkdir -p ~/bin
  wget -O ~/bin/bazel "${url}"
  chmod +x ~/bin/bazel
  PATH="${HOME}/bin:${PATH}"

  git clone https://github.com/bazelbuild/bazel.git
  cd bazel
  git pull --tags
  git checkout "${tag}"
  cd -
}

install_fuse() {
  case "${TRAVIS_OS_NAME}" in
    linux)
      sudo apt-get update
      sudo apt-get install -qq fuse libfuse-dev pkg-config user-mode-linux

      sudo usermod -a -G fuse "${USER}"

      sudo /bin/sh -c 'echo user_allow_other >>/etc/fuse.conf'
      sudo chmod 644 /etc/fuse.conf
      ;;

    osx)
      brew update
      brew cask install osxfuse

      sudo /Library/Filesystems/osxfuse.fs/Contents/Resources/load_osxfuse
      sudo sysctl -w vfs.generic.osxfuse.tunables.allow_other=1
      ;;

    *)
      echo "Don't know how to install FUSE for OS ${TRAVIS_OS_NAME}" 1>&2
      exit 1
      ;;
  esac
}

install_gperftools() {
  case "${TRAVIS_OS_NAME}" in
    linux)
      # Assume install_fuse has already run, which updates the packages
      # repository and also installs pkg-config.
      sudo apt-get install -qq libgoogle-perftools-dev
      ;;

    *)
      echo "Don't know how to install gperftools for OS ${TRAVIS_OS_NAME}" 1>&2
      exit 1
      ;;
  esac
}

install_rust() {
  # We need to manually install Rust because we can only specify a single
  # language in .travis.yml, and that language is Go for now.
  curl https://sh.rustup.rs -sSf | sh -s -- -y
  PATH="${HOME}/.cargo/bin:${PATH}"
}

case "${DO}" in
  bazel)
    install_bazel
    install_fuse
    install_rust
    ;;

  install|test)
    install_fuse
    install_rust
    if [ "${FEATURES}" = profiling ]; then
      install_gperftools
    fi
    ;;

  lint)
    install_fuse  # Needed by Clippy to build the fuse Rust dependency.
    install_rust
    rustup component add clippy-preview
    ;;
esac
