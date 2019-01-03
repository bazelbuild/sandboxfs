#! /bin/sh
# Copyright 2019 Google Inc.
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

# Builds a self-installer package for macOS.
#
# This assumes Cargo, OSXFUSE, and pkg-config are installed.
#
# All arguments given to this script are delegated to "configure".


# Directory name of the running script.
DirName="$(dirname "${0}")"


# Base name of the running script.
ProgName="${0##*/}"


# Prints the given error message to stderr and exits.
#
# \param ... The message to print.
err() {
    echo "${ProgName}: E: $*" 1>&2
    exit 1
}


# Prints the given informational message to stderr.
#
# \param ... The message to print.
info() {
    echo "${ProgName}: I: $*" 1>&2
}


# Modifies the fresh sandboxfs installation for our packaging needs.
#
# \param root Path to the new file system root used to build the package.
configure_root() {
    local root="${1}"; shift

    mkdir -p "${root}/etc/paths.d"
    cat >"${root}/etc/paths.d/sandboxfs" <<EOF
/usr/local/bin
EOF

    mkdir -p "${root}/usr/local/libexec/sandboxfs"
    cat >"${root}/usr/local/libexec/sandboxfs/uninstall.sh" <<EOF
#! /bin/sh

cd /
for f in \$(tail -r /usr/local/share/sandboxfs/manifest); do
    if [ ! -d "\${f}" ]; then
        rm "\${f}"
    else
        # Some directories are shared with other packages (like
        # /usr/local/bin) so just ignore errors during their removal.
        rmdir "\${f}" 2>/dev/null || true
    fi
done
EOF
    chmod +x "${root}/usr/local/libexec/sandboxfs/uninstall.sh"

    mkdir -p "${root}/usr/local/share/sandboxfs"
    ( cd "${root}" && find . >"${root}/usr/local/share/sandboxfs/manifest" )
}


# Program's entry point.
main() {
    [ "$(uname -s)" = Darwin ] || err "This script is for macOS only"
    [ -x "${DirName}/../configure" ] || err "configure not found; make" \
        "sure to run this from a cloned repository"

    local tempdir
    tempdir="$(mktemp -d "${TMPDIR:-/tmp}/${ProgName}.XXXXXX" 2>/dev/null)" \
        || err "Failed to create temporary directory"
    trap "rm -rf '${tempdir}'" EXIT

    export PKG_CONFIG_PATH=/usr/local/lib/pkgconfig  # For OSXFUSE.

    info "Cloning fresh copy of the source tree"
    git clone "${DirName}/.." "${tempdir}/src"

    info "Building and installing into temporary root"
    (
        set -e
        cd "${tempdir}/src"
        ./configure --goroot=none --prefix=/usr/local "${@}"
        make release
        make install DESTDIR="${tempdir}/root"
    ) || err "Build failed"

    info "Preparing temporary root for packaging"
    configure_root "${tempdir}/root"

    local version="$(grep ^version "${tempdir}/src/Cargo.toml" \
        | cut -d '"' -f 2)"
    local revision="$(date +%Y%m%d)"
    local pkgversion="${version}-${revision}"
    local pkgfile="sandboxfs-${pkgversion}-macos.pkg"

    info "Building package ${pkgfile}"
    ( cd "${tempdir}/root" && find . ) | sed 's,^,MANIFEST: ,'
    pkgbuild \
        --identifier com.github.bazelbuild.sandboxfs \
        --root "${tempdir}/root" \
        --version "${pkgversion}" \
        "${pkgfile}"
}


main "${@}"
