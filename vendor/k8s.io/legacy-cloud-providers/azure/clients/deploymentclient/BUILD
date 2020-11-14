load("@io_bazel_rules_go//go:def.bzl", "go_library")

go_library(
    name = "go_default_library",
    srcs = [
        "azure_deploymentclient.go",
        "doc.go",
        "interface.go",
    ],
    importmap = "sigs.k8s.io/cloud-provider-azure/vendor/k8s.io/legacy-cloud-providers/azure/clients/deploymentclient",
    importpath = "k8s.io/legacy-cloud-providers/azure/clients/deploymentclient",
    visibility = ["//visibility:public"],
    deps = [
        "//vendor/github.com/Azure/azure-sdk-for-go/services/resources/mgmt/2017-05-10/resources:go_default_library",
        "//vendor/github.com/Azure/go-autorest/autorest:go_default_library",
        "//vendor/github.com/Azure/go-autorest/autorest/azure:go_default_library",
        "//vendor/github.com/Azure/go-autorest/autorest/to:go_default_library",
        "//vendor/k8s.io/client-go/util/flowcontrol:go_default_library",
        "//vendor/k8s.io/klog/v2:go_default_library",
        "//vendor/k8s.io/legacy-cloud-providers/azure/clients:go_default_library",
        "//vendor/k8s.io/legacy-cloud-providers/azure/clients/armclient:go_default_library",
        "//vendor/k8s.io/legacy-cloud-providers/azure/metrics:go_default_library",
        "//vendor/k8s.io/legacy-cloud-providers/azure/retry:go_default_library",
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
        "//staging/src/k8s.io/legacy-cloud-providers/azure/clients/deploymentclient/mockdeploymentclient:all-srcs",
    ],
    tags = ["automanaged"],
    visibility = ["//visibility:public"],
)
