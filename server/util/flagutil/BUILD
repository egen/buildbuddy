load("@io_bazel_rules_go//go:def.bzl", "go_library", "go_test")

go_library(
    name = "flagutil",
    srcs = ["flagutil.go"],
    importpath = "github.com/buildbuddy-io/buildbuddy/server/util/flagutil",
    visibility = ["//visibility:public"],
    deps = ["//server/util/alert"],
)

go_test(
    name = "flagutil_test",
    srcs = ["flagutil_test.go"],
    embed = [":flagutil"],
    deps = [
        "@com_github_stretchr_testify//assert",
        "@com_github_stretchr_testify//require",
    ],
)
