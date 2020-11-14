package(default_visibility = ["//visibility:public"])

load(
    "@io_bazel_rules_go//go:def.bzl",
    "go_library",
)

go_library(
    name = "go_default_library",
    srcs = [
        "doc.go",
        "ports.go",
    ],
    importmap = "sigs.k8s.io/cloud-provider-azure/vendor/k8s.io/kubernetes/pkg/master/ports",
    importpath = "k8s.io/kubernetes/pkg/master/ports",
    deps = [
        "//vendor/k8s.io/cloud-provider:go_default_library",
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
