package(default_visibility = ["//visibility:public"])

load(
    "@io_bazel_rules_go//go:def.bzl",
    "go_library",
)

go_library(
    name = "go_default_library",
    srcs = [
        "ciphersuites_flag.go",
        "ciphersuites_flag_114.go",
        "colon_separated_multimap_string_string.go",
        "configuration_map.go",
        "flags.go",
        "langle_separated_map_string_string.go",
        "map_string_bool.go",
        "map_string_string.go",
        "namedcertkey_flag.go",
        "noop.go",
        "omitempty.go",
        "sectioned.go",
        "string_flag.go",
        "tristate.go",
    ],
    importmap = "sigs.k8s.io/cloud-provider-azure/vendor/k8s.io/component-base/cli/flag",
    importpath = "k8s.io/component-base/cli/flag",
    deps = [
        "//vendor/github.com/spf13/pflag:go_default_library",
        "//vendor/k8s.io/apimachinery/pkg/util/sets:go_default_library",
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
)
