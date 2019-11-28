#!/usr/bin/env bash

trap 'git checkout --force master' EXIT

git fetch --all
git rebase upstream/master k8s-master
git checkout k8s-master
git subtree split --prefix prow --branch k8s-prow
git rebase k8s-prow master
git push origin master --force