workspace(name = "sandboxfs")

http_archive(
    name = "io_bazel_rules_go",
    url = "https://github.com/bazelbuild/rules_go/releases/download/0.9.0/rules_go-0.9.0.tar.gz",
    sha256 = "4d8d6244320dd751590f9100cf39fd7a4b75cd901e1f3ffdfd6f048328883695",
)

# TODO(jmmv): Drop in favor of a new rules_go release once the commit below is
# part of one.
git_repository(
    name = "com_github_bazelbuild_rules_go",
    remote = "https://github.com/bazelbuild/rules_go.git",
    commit = "dd3c631c31c8d4e3b5bcffc4b66e8c092172ed89",
)

http_archive(
    name = "bazel_gazelle",
    url = "https://github.com/bazelbuild/bazel-gazelle/releases/download/0.9/bazel-gazelle-0.9.tar.gz",
    sha256 = "0103991d994db55b3b5d7b06336f8ae355739635e0c2379dea16b8213ea5a223",
)

load(
    "@io_bazel_rules_go//go:def.bzl",
    "go_register_toolchains",
    "go_repository",
    "go_rules_dependencies",
)

load("@bazel_gazelle//:deps.bzl", "gazelle_dependencies")

go_register_toolchains(go_version = "host")

go_repository(
    name = "bazel_gopath",
    importpath = "github.com/DarkDNA/bazel-gopath",
    commit = "f83ae8d403d0c826335a5edf4bbd1f7b0cf176e4",

    # bazel-gopath ships with a proto file and also a precompiled version of it.
    # The proto file does not include the right Go options, which confuses
    # Gazelle, so prefer the precompiled version.
    build_file_proto_mode = "disable",
)

go_repository(
    name = "golint",
    importpath = "github.com/golang/lint",
    commit = "6aaf7c34af0f4c36a57e0c429bace4d706d8e931",
)

go_repository(
    name = "org_bazil_fuse",
    importpath = "bazil.org/fuse",
    commit = "371fbbdaa8987b715bdd21d6adc4c9b20155f748",
)

go_repository(
    name = "org_golang_x_sys",
    importpath = "golang.org/x/sys",
    commit = "4b45465282a4624cf39876842a017334f13b8aff",
)

gazelle_dependencies()
go_rules_dependencies()
