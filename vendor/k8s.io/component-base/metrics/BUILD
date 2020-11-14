load("@io_bazel_rules_go//go:def.bzl", "go_library")

go_library(
    name = "go_default_library",
    srcs = [
        "collector.go",
        "counter.go",
        "desc.go",
        "gauge.go",
        "histogram.go",
        "http.go",
        "labels.go",
        "metric.go",
        "options.go",
        "opts.go",
        "processstarttime.go",
        "registry.go",
        "summary.go",
        "value.go",
        "version.go",
        "version_parser.go",
        "wrappers.go",
    ],
    importmap = "sigs.k8s.io/cloud-provider-azure/vendor/k8s.io/component-base/metrics",
    importpath = "k8s.io/component-base/metrics",
    visibility = ["//visibility:public"],
    deps = [
        "//vendor/github.com/blang/semver:go_default_library",
        "//vendor/github.com/prometheus/client_golang/prometheus:go_default_library",
        "//vendor/github.com/prometheus/client_golang/prometheus/promhttp:go_default_library",
        "//vendor/github.com/prometheus/client_model/go:go_default_library",
        "//vendor/github.com/prometheus/procfs:go_default_library",
        "//vendor/github.com/spf13/pflag:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/version:go_default_library",
        "//vendor/k8s.io/component-base/version:go_default_library",
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
        "//staging/src/k8s.io/component-base/metrics/legacyregistry:all-srcs",
        "//staging/src/k8s.io/component-base/metrics/prometheus/clientgo:all-srcs",
        "//staging/src/k8s.io/component-base/metrics/prometheus/ratelimiter:all-srcs",
        "//staging/src/k8s.io/component-base/metrics/prometheus/restclient:all-srcs",
        "//staging/src/k8s.io/component-base/metrics/prometheus/version:all-srcs",
        "//staging/src/k8s.io/component-base/metrics/prometheus/workqueue:all-srcs",
        "//staging/src/k8s.io/component-base/metrics/testutil:all-srcs",
    ],
    tags = ["automanaged"],
    visibility = ["//visibility:public"],
)

# Packages allowed to import github.com/prometheus
package_group(
    name = "prometheus_import_allow_list",
    packages = [
        "//cluster/images/etcd-version-monitor",
        "//pkg/scheduler/framework/v1alpha1",
        "//pkg/volume/util/operationexecutor",
        "//staging/src/k8s.io/apiserver/pkg/util/flowcontrol/metrics",
        "//staging/src/k8s.io/component-base/metrics/...",
        "//test/e2e_node",
        "//test/integration/apiserver/flowcontrol",
        "//vendor/...",
    ],
)
