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

package clone

import (
	"reflect"
	"testing"

	"k8s.io/test-infra/prow/kube"
)

func TestParseRefs(t *testing.T) {
	var testCases = []struct {
		name      string
		value     string
		expected  kube.Refs
		expectErr bool
	}{
		{
			name:  "base branch only",
			value: "org,repo=branch",
			expected: kube.Refs{
				Org:     "org",
				Repo:    "repo",
				BaseRef: "branch",
			},
			expectErr: false,
		},
		{
			name:  "base branch and sha",
			value: "org,repo=branch:sha",
			expected: kube.Refs{
				Org:     "org",
				Repo:    "repo",
				BaseRef: "branch",
				BaseSHA: "sha",
			},
			expectErr: false,
		},
		{
			name:  "base branch and pr number only",
			value: "org,repo=branch,1",
			expected: kube.Refs{
				Org:     "org",
				Repo:    "repo",
				BaseRef: "branch",
				Pulls:   []kube.Pull{{Number: 1}},
			},
			expectErr: false,
		},
		{
			name:  "base branch and pr number and sha",
			value: "org,repo=branch,1:sha",
			expected: kube.Refs{
				Org:     "org",
				Repo:    "repo",
				BaseRef: "branch",
				Pulls:   []kube.Pull{{Number: 1, SHA: "sha"}},
			},
			expectErr: false,
		},
		{
			name:  "base branch, sha, pr number and sha",
			value: "org,repo=branch:sha,1:pull-sha",
			expected: kube.Refs{
				Org:     "org",
				Repo:    "repo",
				BaseRef: "branch",
				BaseSHA: "sha",
				Pulls:   []kube.Pull{{Number: 1, SHA: "pull-sha"}},
			},
			expectErr: false,
		},
		{
			name:  "base branch and multiple prs",
			value: "org,repo=branch,1,2,3",
			expected: kube.Refs{
				Org:     "org",
				Repo:    "repo",
				BaseRef: "branch",
				Pulls:   []kube.Pull{{Number: 1}, {Number: 2}, {Number: 3}},
			},
			expectErr: false,
		},
		{
			name:  "base branch and multiple prs with shas",
			value: "org,repo=branch:sha,1:pull-1-sha,2:pull-2-sha,3:pull-3-sha",
			expected: kube.Refs{
				Org:     "org",
				Repo:    "repo",
				BaseRef: "branch",
				BaseSHA: "sha",
				Pulls:   []kube.Pull{{Number: 1, SHA: "pull-1-sha"}, {Number: 2, SHA: "pull-2-sha"}, {Number: 3, SHA: "pull-3-sha"}},
			},
			expectErr: false,
		},
		{
			name:      "no org or repo",
			value:     "branch:sha",
			expectErr: true,
		},
		{
			name:      "no repo",
			value:     "org=branch",
			expectErr: true,
		},
		{
			name:      "no refs",
			value:     "org,repo=",
			expectErr: true,
		},
		{
			name:      "malformed base ref",
			value:     "org,repo=branch:whatever:sha",
			expectErr: true,
		},
		{
			name:      "malformed pull ref",
			value:     "org,repo=branch:sha,1:what:ever",
			expectErr: true,
		},
		{
			name:      "malformed pull nuber",
			value:     "org,repo=branch:sha,NaN:sha",
			expectErr: true,
		},
	}

	for _, testCase := range testCases {
		actual, err := ParseRefs(testCase.value)
		if testCase.expectErr && err == nil {
			t.Errorf("%s: expected an error but got none", testCase.name)
		}
		if !testCase.expectErr && err != nil {
			t.Errorf("%s: expected no error but got %v", testCase.name, err)
		}

		if !testCase.expectErr && !reflect.DeepEqual(actual, testCase.expected) {
			t.Errorf("%s: incorrect refs parsed:\nexpected\n\t%v,\ngot:\n\t%v", testCase.name, testCase.expected, actual)
		}
	}
}

func TestParseAliases(t *testing.T) {
	var testCases = []struct {
		name      string
		value     string
		expected  PathResolver
		expectErr bool
	}{
		{
			name:  "org and repo provided",
			value: "org,repo=path",
			expected: PathResolver{
				org:  "org",
				repo: "repo",
				path: "path",
			},
			expectErr: false,
		},
		{
			name:  "only org provided",
			value: "org=path",
			expected: PathResolver{
				org:  "org",
				path: "path",
			},
			expectErr: false,
		},
		{
			name:      "no org or repo",
			value:     "path",
			expectErr: true,
		},
		{
			name:      "no path",
			value:     "org,repo",
			expectErr: true,
		},
	}

	for _, testCase := range testCases {
		actual, err := ParseAliases(testCase.value)
		if testCase.expectErr && err == nil {
			t.Errorf("%s: expected an error but got none", testCase.name)
		}
		if !testCase.expectErr && err != nil {
			t.Errorf("%s: expected no error but got %v", testCase.name, err)
		}

		if !testCase.expectErr && !reflect.DeepEqual(actual, testCase.expected) {
			t.Errorf("%s: incorrect path resolver parsed:\nexpected\n\t%v,\ngot:\n\t%v", testCase.name, testCase.expected, actual)
		}
	}
}

func TestPathResolver_Resolve(t *testing.T) {
	var testCases = []struct {
		name     string
		org      string
		repo     string
		resolver PathResolver
		expected string
	}{
		{
			name: "matching resolver for org and repo",
			org:  "org",
			repo: "repo",
			resolver: PathResolver{
				org:  "org",
				repo: "repo",
				path: "path",
			},
			expected: "path",
		},
		{
			name: "not matching resolver for org and repo",
			org:  "org",
			repo: "repo2",
			resolver: PathResolver{
				org:  "org",
				repo: "repo",
				path: "path",
			},
			expected: "",
		},
		{
			name: "matching resolver for org",
			org:  "org",
			repo: "repo",
			resolver: PathResolver{
				org:  "org",
				path: "path",
			},
			expected: "path",
		},
		{
			name: "not matching resolver for org",
			org:  "org2",
			repo: "repo",
			resolver: PathResolver{
				org:  "org",
				path: "path",
			},
			expected: "",
		},
	}

	for _, testCase := range testCases {
		if actual, expected := testCase.resolver.Resolve(testCase.org, testCase.repo), testCase.expected; actual != expected {
			t.Errorf("%s: incorrect override path resolved:\nexpected\n\t%v,\ngot:\n\t%v", testCase.name, expected, actual)
		}
	}
}
