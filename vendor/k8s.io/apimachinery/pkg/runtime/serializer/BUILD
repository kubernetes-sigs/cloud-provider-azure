package(default_visibility = ["//visibility:public"])

load(
    "@io_bazel_rules_go//go:def.bzl",
    "go_library",
)

go_library(
    name = "go_default_library",
    srcs = [
        "codec_factory.go",
        "negotiated_codec.go",
    ],
    importmap = "sigs.k8s.io/cloud-provider-azure/vendor/k8s.io/apimachinery/pkg/runtime/serializer",
    importpath = "k8s.io/apimachinery/pkg/runtime/serializer",
    deps = [
        "//vendor/k8s.io/apimachinery/pkg/runtime:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/runtime/schema:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/runtime/serializer/json:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/runtime/serializer/protobuf:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/runtime/serializer/recognizer:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/runtime/serializer/versioning:go_default_library",
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
        "//staging/src/k8s.io/apimachinery/pkg/runtime/serializer/json:all-srcs",
        "//staging/src/k8s.io/apimachinery/pkg/runtime/serializer/protobuf:all-srcs",
        "//staging/src/k8s.io/apimachinery/pkg/runtime/serializer/recognizer:all-srcs",
        "//staging/src/k8s.io/apimachinery/pkg/runtime/serializer/streaming:all-srcs",
        "//staging/src/k8s.io/apimachinery/pkg/runtime/serializer/versioning:all-srcs",
        "//staging/src/k8s.io/apimachinery/pkg/runtime/serializer/yaml:all-srcs",
    ],
    tags = ["automanaged"],
)
