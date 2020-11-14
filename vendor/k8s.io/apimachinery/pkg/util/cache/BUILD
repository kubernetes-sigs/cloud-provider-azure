package(default_visibility = ["//visibility:public"])

load(
    "@io_bazel_rules_go//go:def.bzl",
    "go_library",
)

go_library(
    name = "go_default_library",
    srcs = [
        "expiring.go",
        "lruexpirecache.go",
    ],
    importmap = "sigs.k8s.io/cloud-provider-azure/vendor/k8s.io/apimachinery/pkg/util/cache",
    importpath = "k8s.io/apimachinery/pkg/util/cache",
    deps = [
        "//vendor/github.com/hashicorp/golang-lru:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/util/clock:go_default_library",
    ],
)

filegroup(
    name = "package-srcs",
    srcs = glob(["**"]),
    tags = ["automanaged"],
    visibility = ["//visibility:private"],
)

filegroup(
    name = "all-srcs",
    srcs = [":package-srcs"],
    tags = ["automanaged"],
)
