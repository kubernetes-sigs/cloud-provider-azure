load("@io_bazel_rules_go//go:def.bzl", "go_library")

go_library(
    name = "go_default_library",
    srcs = [
        "interface.go",
        "metadata.go",
    ],
    importmap = "sigs.k8s.io/cloud-provider-azure/vendor/k8s.io/client-go/metadata",
    importpath = "k8s.io/client-go/metadata",
    visibility = ["//visibility:public"],
    deps = [
        "//vendor/k8s.io/apimachinery/pkg/apis/meta/internalversion/scheme:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/apis/meta/v1:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/runtime:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/runtime/schema:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/runtime/serializer:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/types:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/watch:go_default_library",
        "//vendor/k8s.io/client-go/rest:go_default_library",
        "//vendor/k8s.io/klog/v2:go_default_library",
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
    srcs = [
        ":package-srcs",
        "//staging/src/k8s.io/client-go/metadata/fake:all-srcs",
        "//staging/src/k8s.io/client-go/metadata/metadatainformer:all-srcs",
        "//staging/src/k8s.io/client-go/metadata/metadatalister:all-srcs",
    ],
    tags = ["automanaged"],
    visibility = ["//visibility:public"],
)
