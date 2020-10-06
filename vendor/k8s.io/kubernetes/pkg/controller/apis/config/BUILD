load("@io_bazel_rules_go//go:def.bzl", "go_library")

go_library(
    name = "go_default_library",
    srcs = [
        "doc.go",
        "register.go",
        "types.go",
        "zz_generated.deepcopy.go",
    ],
    importmap = "sigs.k8s.io/cloud-provider-azure/vendor/k8s.io/kubernetes/pkg/controller/apis/config",
    importpath = "k8s.io/kubernetes/pkg/controller/apis/config",
    visibility = ["//visibility:public"],
    deps = [
        "//vendor/k8s.io/apimachinery/pkg/apis/meta/v1:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/runtime:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/runtime/schema:go_default_library",
        "//vendor/k8s.io/component-base/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/certificates/signer/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/daemon/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/deployment/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/endpoint/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/endpointslice/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/endpointslicemirroring/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/garbagecollector/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/job/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/namespace/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/nodeipam/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/nodelifecycle/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/podautoscaler/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/podgc/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/replicaset/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/replication/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/resourcequota/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/service/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/serviceaccount/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/statefulset/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/ttlafterfinished/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/volume/attachdetach/config:go_default_library",
        "//vendor/k8s.io/kubernetes/pkg/controller/volume/persistentvolume/config:go_default_library",
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
        "//pkg/controller/apis/config/fuzzer:all-srcs",
        "//pkg/controller/apis/config/scheme:all-srcs",
        "//pkg/controller/apis/config/v1alpha1:all-srcs",
    ],
    tags = ["automanaged"],
    visibility = ["//visibility:public"],
)
