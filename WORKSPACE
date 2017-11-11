workspace(name = "sandboxfs")

http_archive(
    name = "io_bazel_rules_go",
    url = "https://github.com/bazelbuild/rules_go/releases/download/0.7.0/rules_go-0.7.0.tar.gz",
    sha256 = "91fca9cf860a1476abdc185a5f675b641b60d3acf0596679a27b580af60bf19c",
)

load(
    "@io_bazel_rules_go//go:def.bzl",
    "go_register_toolchains",
    "go_repository",
    "go_rules_dependencies",
)

go_register_toolchains(go_version = "host")

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

go_rules_dependencies()
