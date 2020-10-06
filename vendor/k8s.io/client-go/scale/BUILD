load("@io_bazel_rules_go//go:def.bzl", "go_library")

go_library(
    name = "go_default_library",
    srcs = [
        "client.go",
        "doc.go",
        "interfaces.go",
        "util.go",
    ],
    importmap = "sigs.k8s.io/cloud-provider-azure/vendor/k8s.io/client-go/scale",
    importpath = "k8s.io/client-go/scale",
    visibility = ["//visibility:public"],
    deps = [
        "//vendor/k8s.io/api/autoscaling/v1:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/api/meta:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/apis/meta/v1:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/runtime:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/runtime/schema:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/runtime/serializer:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/types:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/util/runtime:go_default_library",
        "//vendor/k8s.io/client-go/discovery:go_default_library",
        "//vendor/k8s.io/client-go/dynamic:go_default_library",
        "//vendor/k8s.io/client-go/rest:go_default_library",
        "//vendor/k8s.io/client-go/scale/scheme:go_default_library",
        "//vendor/k8s.io/client-go/scale/scheme/appsint:go_default_library",
        "//vendor/k8s.io/client-go/scale/scheme/appsv1beta1:go_default_library",
        "//vendor/k8s.io/client-go/scale/scheme/appsv1beta2:go_default_library",
        "//vendor/k8s.io/client-go/scale/scheme/autoscalingv1:go_default_library",
        "//vendor/k8s.io/client-go/scale/scheme/extensionsint:go_default_library",
        "//vendor/k8s.io/client-go/scale/scheme/extensionsv1beta1:go_default_library",
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
        "//staging/src/k8s.io/client-go/scale/fake:all-srcs",
        "//staging/src/k8s.io/client-go/scale/scheme:all-srcs",
    ],
    tags = ["automanaged"],
    visibility = ["//visibility:public"],
)
