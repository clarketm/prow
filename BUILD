package(default_visibility = ["//visibility:public"])

load("@io_bazel_rules_docker//docker:docker.bzl", "docker_bundle")
load("@io_bazel_rules_docker//contrib:push-all.bzl", "docker_push")
load(
    ":prow.bzl",
    prow_tags = "tags",
)

docker_bundle(
    name = "release",
    images = prow_tags(
        "branchprotector",
        "clonerefs",
        "deck",
        "hook",
        "horologium",
        "initupload",
        "jenkins-operator",
        "plank",
        "sinker",
        "splice",
        "tide",
        "tot",
    ),
    stamp = True,
)

docker_push(
    name = "release-push",
    bundle = ":release",
)

filegroup(
    name = "configs",
    srcs = glob(["*.yaml"]),
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
        "//prow/cluster:all-srcs",
        "//prow/cmd/branchprotector:all-srcs",
        "//prow/cmd/clonerefs:all-srcs",
        "//prow/cmd/config:all-srcs",
        "//prow/cmd/deck:all-srcs",
        "//prow/cmd/hook:all-srcs",
        "//prow/cmd/horologium:all-srcs",
        "//prow/cmd/initupload:all-srcs",
        "//prow/cmd/jenkins-operator:all-srcs",
        "//prow/cmd/mkpj:all-srcs",
        "//prow/cmd/phony:all-srcs",
        "//prow/cmd/plank:all-srcs",
        "//prow/cmd/sinker:all-srcs",
        "//prow/cmd/splice:all-srcs",
        "//prow/cmd/tide:all-srcs",
        "//prow/cmd/tot:all-srcs",
        "//prow/commentpruner:all-srcs",
        "//prow/config:all-srcs",
        "//prow/cron:all-srcs",
        "//prow/external-plugins/needs-rebase:all-srcs",
        "//prow/genfiles:all-srcs",
        "//prow/git:all-srcs",
        "//prow/github:all-srcs",
        "//prow/hook:all-srcs",
        "//prow/jenkins:all-srcs",
        "//prow/kube:all-srcs",
        "//prow/metrics:all-srcs",
        "//prow/phony:all-srcs",
        "//prow/pjutil:all-srcs",
        "//prow/plank:all-srcs",
        "//prow/pluginhelp:all-srcs",
        "//prow/plugins:all-srcs",
        "//prow/pod-utils/clone:all-srcs",
        "//prow/pod-utils/gcs:all-srcs",
        "//prow/repoowners:all-srcs",
        "//prow/report:all-srcs",
        "//prow/slack:all-srcs",
        "//prow/tide:all-srcs",
    ],
    tags = ["automanaged"],
)
