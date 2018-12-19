workspace(name = "sandboxfs")

load("@bazel_tools//tools/build_defs/repo:git.bzl", "git_repository")
load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

# Needed by, at least, com_github_bazelbuild_buildtools.
# TODO(https://github.com/bazelbuild/bazel/issues/1943): Remove once recursive
# workspaces are supported, unless we actually have to reference this directly.
git_repository(
    name = "io_bazel",
    remote = "https://github.com/bazelbuild/bazel.git",
    tag = "0.20.0",
)

git_repository(
    name = "com_github_bazelbuild_buildtools",
    remote = "https://github.com/bazelbuild/buildtools.git",
    commit = "ab1d6a0ca532b7b7f3450a42d5cbcfdcd736fd41",
)

# Needed by, at least, org_golang_x_lint.
# TODO(https://github.com/bazelbuild/bazel/issues/1943): Remove once recursive
# workspaces are supported, unless we actually have to reference this directly.
git_repository(
    name = "bazel_skylib",
    remote = "https://github.com/bazelbuild/bazel-skylib.git",
    commit = "c00ef493869e2966d47508e8625aae723a4a3054",
)

http_archive(
    name = "io_bazel_rules_go",
    urls = ["https://github.com/bazelbuild/rules_go/releases/download/0.16.4/rules_go-0.16.4.tar.gz"],
    sha256 = "62ec3496a00445889a843062de9930c228b770218c735eca89c67949cd967c3f",
)

http_archive(
    name = "bazel_gazelle",
    urls = ["https://github.com/bazelbuild/bazel-gazelle/releases/download/0.15.0/bazel-gazelle-0.15.0.tar.gz"],
    sha256 = "6e875ab4b6bf64a38c352887760f21203ab054676d9c1b274963907e0768740d",
)

load(
    "@io_bazel_rules_go//go:def.bzl",
    "go_register_toolchains",
    "go_rules_dependencies",
)

load("@bazel_gazelle//:deps.bzl", "gazelle_dependencies", "go_repository")

go_register_toolchains(go_version = "host")

go_repository(
    name = "bazel_gopath",
    importpath = "github.com/DarkDNA/bazel-gopath",
    commit = "46f9d0fc6e529be2f95558449fe7e181934124ee",

    # bazel-gopath ships with a proto file and also a precompiled version of it.
    # The proto file does not include the right Go options, which confuses
    # Gazelle, so prefer the precompiled version.
    build_file_proto_mode = "disable",
)

go_repository(
    name = "org_bazil_fuse",
    importpath = "bazil.org/fuse",
    commit = "65cc252bf6691cb3c7014bcb2c8dc29de91e3a7e",
)

go_repository(
    name = "org_golang_x_lint",
    importpath = "golang.org/x/lint",
    commit = "93c0bb5c83939f89e6238cefd42de38f33734409",
)

go_repository(
    name = "org_golang_x_sys",
    importpath = "golang.org/x/sys",
    commit = "4d1cda033e0619309c606fc686de3adcf599539e",
)

gazelle_dependencies()
go_rules_dependencies()
