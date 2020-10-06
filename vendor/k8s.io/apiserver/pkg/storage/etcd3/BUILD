package(default_visibility = ["//visibility:public"])

load(
    "@io_bazel_rules_go//go:def.bzl",
    "go_library",
)

go_library(
    name = "go_default_library",
    srcs = [
        "api_object_versioner.go",
        "compact.go",
        "errors.go",
        "event.go",
        "healthcheck.go",
        "lease_manager.go",
        "logger.go",
        "store.go",
        "watcher.go",
    ],
    importmap = "sigs.k8s.io/cloud-provider-azure/vendor/k8s.io/apiserver/pkg/storage/etcd3",
    importpath = "k8s.io/apiserver/pkg/storage/etcd3",
    deps = [
        "//vendor/go.etcd.io/etcd/clientv3:go_default_library",
        "//vendor/go.etcd.io/etcd/etcdserver/api/v3rpc/rpctypes:go_default_library",
        "//vendor/go.etcd.io/etcd/mvcc/mvccpb:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/api/errors:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/api/meta:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/apis/meta/v1:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/apis/meta/v1/unstructured:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/conversion:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/runtime:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/util/runtime:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/util/validation/field:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/watch:go_default_library",
        "//vendor/k8s.io/apiserver/pkg/features:go_default_library",
        "//vendor/k8s.io/apiserver/pkg/storage:go_default_library",
        "//vendor/k8s.io/apiserver/pkg/storage/etcd3/metrics:go_default_library",
        "//vendor/k8s.io/apiserver/pkg/storage/value:go_default_library",
        "//vendor/k8s.io/apiserver/pkg/util/feature:go_default_library",
        "//vendor/k8s.io/klog/v2:go_default_library",
        "//vendor/k8s.io/utils/trace:go_default_library",
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
        "//staging/src/k8s.io/apiserver/pkg/storage/etcd3/metrics:all-srcs",
        "//staging/src/k8s.io/apiserver/pkg/storage/etcd3/preflight:all-srcs",
        "//staging/src/k8s.io/apiserver/pkg/storage/etcd3/testing:all-srcs",
    ],
    tags = ["automanaged"],
)
