load("@rules_foreign_cc//foreign_cc:defs.bzl", "configure_make")
load("@rules_cc//cc:defs.bzl", "cc_binary", "cc_library")

exports_files([
    "vmlinux",
    "initrd.cpio",
])

configure_make(
    name = "cpio",
    configure_in_place = False,
    lib_source = "@org_gnu_cpio//:all",
    out_binaries = ["cpio"],
    tags = ["manual"],
)

genrule(
    name = "mkinitrd",
    srcs = [
        "//enterprise/server/cmd/goinit",
        "//enterprise/server/vmvfs",
    ],
    outs = ["initrd-latest.cpio"],
    cmd_bash = """
        ./$(location mkinitrd.sh) \
            $(location //enterprise/server/cmd/goinit:goinit) \
            $(location //enterprise/server/vmvfs) \
            \"$@\"
    """,
    tags = ["manual"],
    tools = [
        "mkinitrd.sh",
        ":cpio",
    ],
    visibility = ["//visibility:public"],
)
