#!/bin/bash
# Copyright 2016 The Kubernetes Authors.
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

if [ "$#" -ne 1 ]; then
  echo "usage: $0 [program]"
  exit 1
fi

# darwin is great
SED=sed
if which gsed &>/dev/null; then
  SED=gsed
fi
if ! ($SED --version 2>&1 | grep -q GNU); then
  echo "!!! GNU sed is required.  If on OS X, use 'brew install gnu-sed'."
  exit 1
fi

cd $(dirname $0)

makefile_version_re="^\(${1}_VERSION.*=\s*\)"
version=$($SED -n "s/$makefile_version_re//Ip" Makefile)
new_version=$(awk -F. '{print $1 "." $2+1}' <<< $version)

echo "program: $1"
echo "old version: $version"
echo "new version: $new_version"

$SED -i "s/$makefile_version_re.*/\1$new_version/I" Makefile
$SED -i "s/\(${1}:\)[0-9.]\+/\1$new_version/I" cluster/*.yaml
