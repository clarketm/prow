#!/bin/bash

# Copyright 2019 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -o errexit
set -o nounset
set -o pipefail

# bump.sh is used to update references to Prow component images hosted at gcr.io/k8s-prow/*
# Specifically it does the following:
# - Optionally activate GOOGLE_APPLICATION_CREDENTIALS and configure-docker if set.
# - Select a new image version to bump to using one of the following:
#   - The version currently used by prow.k8s.io:  ./bump.sh --upstream
#   - An explicitly specified tag:  ./bump.sh v20191004-b2c87e85c
#   - The latest available tag: ./bump.sh --latest
# - Update the version of all gcr.io/k8s-prow/* images in the bumpfiles identified below.
#   - IMPORTANT: The bumpfile paths need to be updated to point to the config files for your Prow instance!

# Identify which files need to be updated. This includes:
# - Prow component deployment files
# - config.yaml (to update pod utility image version in plank's default decoration config)
# - Any job config files that reference Prow images (e.g. branchprotector, peribolos, config-bootstrapper)
#   - NOTE: This script only update gcr.io/k8s-prow/* images so it is safe to run on the entire job config.
#   - NOTE: If you define all ProwJob config in config.yaml you can omit this entirely.
COMPONENT_FILE_DIR="${COMPONENT_FILE_DIR:-}"
CONFIG_PATH="${CONFIG_PATH:-}"
JOB_CONFIG_PATH="${JOB_CONFIG_PATH:-}"

usage() {
  echo "Usage: $(basename "$0") [--list || --latest || --upstream || vYYYYMMDD-deadbeef]" >&2
  exit 1
}

main() {
  check-args
  check-requirements
  cd "$(git rev-parse --show-toplevel)"

  # Determine the new_version to bump to based on the mode.
  cmd=
  if [[ $# != 0 ]]; then
    cmd="$1"
  fi
  if [[ -z "${cmd}" || "${cmd}" == "--list" ]]; then
    list
  elif [[ "${cmd}" =~ v[0-9]{8}-[a-f0-9]{6,9} ]]; then
    new_version="${cmd}"
  elif [[ "${cmd}" == "--latest" ]]; then
    new_version="$(list-options 1)"
  elif [[ "${cmd}" == "--upstream" ]]; then
    new_version="$(upstream-version)"
  else
    usage
  fi
  echo -e "Bumping: 'gcr.io/k8s-prow/' images to $(color-version ${new_version}) ..." >&2

  bumpfiles=()
  bumpfiles+=("${COMPONENT_FILE_DIR}"/*.yaml)
  bumpfiles+=("${CONFIG_PATH}")
  if [[ -n "${JOB_CONFIG_PATH}" ]]; then
    bumpfiles+=($(grep -rl -e "gcr.io/k8s-prow/" "${JOB_CONFIG_PATH}"))
  fi

  echo -e "Attempting to bump the following files: ${bumpfiles[@]}" >&2

  # Update image tags in the identified files.
  filter="s/gcr.io\/k8s-prow\/\([[:alnum:]_-]\+\):v[a-f0-9-]\+/gcr.io\/k8s-prow\/\1:${new_version}/I"
  for file in "${bumpfiles[@]}"; do
    ${SED} -i "${filter}" ${file}
  done

  echo "bump.sh completed successfully!" >&2
}

check-args() {
  if [[ -z "${COMPONENT_FILE_DIR}" ]]; then
    echo "ERROR: $COMPONENT_FILE_DIR must be specified." >&2
  fi
  if [[ -z "${CONFIG_PATH}" ]]; then
    echo "ERROR: $CONFIG_PATH must be specified." >&2
  fi
  if [[ -z "${JOB_CONFIG_PATH}" ]]; then
    echo "ERROR: $JOB_CONFIG_PATH must be specified." >&2
  fi
}

check-requirements() {
  if command -v gsed &>/dev/null; then
    SED="gsed"
  else
    SED="sed"
  fi

  if ! (${SED} --version 2>&1 | grep -q GNU); then
    # darwin is great (not)
    echo "!!! GNU sed is required.  If on OS X, use 'brew install gnu-sed'." >&2
    exit 1
  fi

  TAC=tac

  if command -v gtac &>/dev/null; then
    TAC=gtac
  fi

  if ! command -v "${TAC}" &>/dev/null; then
    echo "tac (reverse cat) required. If on OS X then 'brew install coreutils'." >&2
    exit 1
  fi

  if [[ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ]]; then
    echo "Detected GOOGLE_APPLICATION_CREDENTIALS, activating..." >&2
    gcloud auth activate-service-account --key-file="${GOOGLE_APPLICATION_CREDENTIALS}"
    gcloud auth configure-docker
  fi
}

# List the $1 most recently pushed prow versions
list-options() {
  local count="$1"
  gcloud container images list-tags gcr.io/k8s-prow/plank --limit="${count}" --format='value(tags)' \
      | grep -o -E 'v[^,]+' | "${TAC}"
}

upstream-version() {
 local branch="https://raw.githubusercontent.com/kubernetes/test-infra/master"
 local file="prow/cluster/deck_deployment.yaml"

 curl "$branch/$file" | grep image: | grep -o -E 'v[-0-9a-f]+'
}

# Print 10 most recent prow versions, ask user to select one, which becomes new_version
list() {
  echo "Listing recent versions..." >&2
  echo "Recent versions of prow:" >&2
  mapfile -t options < <(list-options 10)
  if [[ -z "${options[*]}" ]]; then
    echo "No versions found" >&2
    exit 1
  fi
  local def_opt=$(upstream-version)
  new_version=
  for o in "${options[@]}"; do
    if [[ "$o" == "$def_opt" ]]; then
      echo -e "  $(color-image "$o"   '*' prow.k8s.io)"
    else
      echo -e "  $(color-version "${o}")"
    fi
  done
  read -rp "Select version [$(color-image "${def_opt}")]: " new_version
  if [[ -z "${new_version:-}" ]]; then
    new_version="${def_opt}"
  else
    local found=
    for o in "${options[@]}"; do
      if [[ "${o}" == "${new_version}" ]]; then
        found=yes
        break
      fi
    done
    if [[ -z "${found}" ]]; then
      echo "Invalid version: ${new_version}" >&2
      exit 1
    fi
  fi
}

# See https://misc.flogisoft.com/bash/tip_colors_and_formatting
color-image() { # Bold magenta
  echo -e "\x1B[1;35m${*}\x1B[0m"
}
color-version() { # Bold blue
  echo -e "\x1B[1;34m${*}\x1B[0m"
}

main "$@"
