/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package github

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// SecurityForkNameRE is a regexp matching repos that are temporary security forks
// https://help.github.com/en/github/managing-security-vulnerabilities/collaborating-in-a-temporary-private-fork-to-resolve-a-security-vulnerability
var SecurityForkNameRE = regexp.MustCompile(`^[\w-]+-ghsa-[\w-]+$`)

// HasLabel checks if label is in the label set "issueLabels".
func HasLabel(label string, issueLabels []Label) bool {
	for _, l := range issueLabels {
		if strings.ToLower(l.Name) == strings.ToLower(label) {
			return true
		}
	}
	return false
}

// hasLabels checks if all labels are in the github.label set "issueLabels"
func HasLabels(labels []string, issueLabels []Label) bool {
	for _, label := range labels {
		if !HasLabel(label, issueLabels) {
			return false
		}
	}
	return true
}

// ImageTooBig checks if image is bigger than github limits
func ImageTooBig(url string) (bool, error) {
	// limit is 10MB
	limit := 10000000
	// try to get the image size from Content-Length header
	resp, err := http.Head(url)
	if err != nil {
		return true, fmt.Errorf("HEAD error: %v", err)
	}
	if sc := resp.StatusCode; sc != http.StatusOK {
		return true, fmt.Errorf("failing %d response", sc)
	}
	size, _ := strconv.Atoi(resp.Header.Get("Content-Length"))
	if size > limit {
		return true, nil
	}
	return false, nil
}

// LevelFromPermissions adapts a repo permissions struct to the
// appropriate permission level used elsewhere
func LevelFromPermissions(permissions RepoPermissions) RepoPermissionLevel {
	if permissions.Admin {
		return Admin
	} else if permissions.Push {
		return Write
	} else if permissions.Pull {
		return Read
	} else {
		return None
	}
}

// PermissionsFromLevel adapts a repo permission level to the
// appropriate permissions struct used elsewhere
func PermissionsFromLevel(permission RepoPermissionLevel) RepoPermissions {
	switch permission {
	case None:
		return RepoPermissions{}
	case Read:
		return RepoPermissions{Pull: true}
	case Write:
		return RepoPermissions{Pull: true, Push: true}
	case Admin:
		return RepoPermissions{Pull: true, Push: true, Admin: true}
	default:
		return RepoPermissions{}
	}
}
