load("@io_bazel_rules_go//go:def.bzl", "go_library")

go_library(
    name = "go_default_library",
    srcs = [
        "doc.go",
        "queueset.go",
        "types.go",
    ],
    importmap = "sigs.k8s.io/cloud-provider-azure/vendor/k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/queueset",
    importpath = "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/queueset",
    visibility = ["//visibility:public"],
    deps = [
        "//vendor/github.com/pkg/errors:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/util/clock:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/util/runtime:go_default_library",
        "//vendor/k8s.io/apiserver/pkg/endpoints/request:go_default_library",
        "//vendor/k8s.io/apiserver/pkg/util/flowcontrol/counter:go_default_library",
        "//vendor/k8s.io/apiserver/pkg/util/flowcontrol/debug:go_default_library",
        "//vendor/k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing:go_default_library",
        "//vendor/k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/promise:go_default_library",
        "//vendor/k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/promise/lockingpromise:go_default_library",
        "//vendor/k8s.io/apiserver/pkg/util/flowcontrol/metrics:go_default_library",
        "//vendor/k8s.io/apiserver/pkg/util/shufflesharding:go_default_library",
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
    srcs = [":package-srcs"],
    tags = ["automanaged"],
    visibility = ["//visibility:public"],
)
