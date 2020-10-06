load("@io_bazel_rules_go//go:def.bzl", "go_library")

go_library(
    name = "go_default_library",
    srcs = [
        "azure_cache.go",
        "doc.go",
    ],
    importmap = "sigs.k8s.io/cloud-provider-azure/vendor/k8s.io/legacy-cloud-providers/azure/cache",
    importpath = "k8s.io/legacy-cloud-providers/azure/cache",
    visibility = ["//visibility:public"],
    deps = ["//vendor/k8s.io/client-go/tools/cache:go_default_library"],
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
    visibility = ["//visibility:public"],
)
