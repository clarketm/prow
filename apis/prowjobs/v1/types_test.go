/*
Copyright 2018 The Kubernetes Authors.

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

package v1

import (
	"reflect"
	"testing"
	"time"
)

func TestDecorationDefaulting(t *testing.T) {
	truth := true
	lies := false

	var testCases = []struct {
		name     string
		provided *DecorationConfig
		// Note: def is a copy of the defaults and may be modified.
		expected func(orig, def *DecorationConfig) *DecorationConfig
	}{
		{
			name:     "nothing provided",
			provided: &DecorationConfig{},
			expected: func(orig, def *DecorationConfig) *DecorationConfig {
				return def
			},
		},
		{
			name: "timeout provided",
			provided: &DecorationConfig{
				Timeout: &Duration{Duration: 10 * time.Minute},
			},
			expected: func(orig, def *DecorationConfig) *DecorationConfig {
				def.Timeout = orig.Timeout
				return def
			},
		},
		{
			name: "grace period provided",
			provided: &DecorationConfig{
				GracePeriod: &Duration{Duration: 10 * time.Hour},
			},
			expected: func(orig, def *DecorationConfig) *DecorationConfig {
				def.GracePeriod = orig.GracePeriod
				return def
			},
		},
		{
			name: "utility images provided",
			provided: &DecorationConfig{
				UtilityImages: &UtilityImages{
					CloneRefs:  "clonerefs-special",
					InitUpload: "initupload-special",
					Entrypoint: "entrypoint-special",
					Sidecar:    "sidecar-special",
				},
			},
			expected: func(orig, def *DecorationConfig) *DecorationConfig {
				def.UtilityImages = orig.UtilityImages
				return def
			},
		},
		{
			name: "gcs configuration provided",
			provided: &DecorationConfig{
				GCSConfiguration: &GCSConfiguration{
					Bucket:       "bucket-1",
					PathPrefix:   "prefix-2",
					PathStrategy: PathStrategyExplicit,
					DefaultOrg:   "org2",
					DefaultRepo:  "repo2",
				},
			},
			expected: func(orig, def *DecorationConfig) *DecorationConfig {
				def.GCSConfiguration = orig.GCSConfiguration
				return def
			},
		},
		{
			name: "secret name provided",
			provided: &DecorationConfig{
				GCSCredentialsSecret: "somethingSecret",
			},
			expected: func(orig, def *DecorationConfig) *DecorationConfig {
				def.GCSCredentialsSecret = orig.GCSCredentialsSecret
				return def
			},
		},
		{
			name: "ssh secrets provided",
			provided: &DecorationConfig{
				SSHKeySecrets: []string{"my", "special"},
			},
			expected: func(orig, def *DecorationConfig) *DecorationConfig {
				def.SSHKeySecrets = orig.SSHKeySecrets
				return def
			},
		},

		{
			name: "utility images partially provided",
			provided: &DecorationConfig{
				UtilityImages: &UtilityImages{
					CloneRefs:  "clonerefs-special",
					InitUpload: "initupload-special",
				},
			},
			expected: func(orig, def *DecorationConfig) *DecorationConfig {
				def.UtilityImages.CloneRefs = orig.UtilityImages.CloneRefs
				def.UtilityImages.InitUpload = orig.UtilityImages.InitUpload
				return def
			},
		},
		{
			name: "gcs configuration partially provided",
			provided: &DecorationConfig{
				GCSConfiguration: &GCSConfiguration{
					Bucket: "bucket-1",
				},
			},
			expected: func(orig, def *DecorationConfig) *DecorationConfig {
				def.GCSConfiguration.Bucket = orig.GCSConfiguration.Bucket
				return def
			},
		},
		{
			name: "skip_cloning provided",
			provided: &DecorationConfig{
				SkipCloning: &lies,
			},
			expected: func(orig, def *DecorationConfig) *DecorationConfig {
				def.SkipCloning = orig.SkipCloning
				return def
			},
		},
		{
			name: "ssh host fingerprints provided",
			provided: &DecorationConfig{
				SSHHostFingerprints: []string{"unique", "print"},
			},
			expected: func(orig, def *DecorationConfig) *DecorationConfig {
				def.SSHHostFingerprints = orig.SSHHostFingerprints
				return def
			},
		},
	}

	for _, testCase := range testCases {
		tc := testCase
		t.Run(tc.name, func(t *testing.T) {
			defaults := &DecorationConfig{
				Timeout:     &Duration{Duration: 1 * time.Minute},
				GracePeriod: &Duration{Duration: 10 * time.Second},
				UtilityImages: &UtilityImages{
					CloneRefs:  "clonerefs",
					InitUpload: "initupload",
					Entrypoint: "entrypoint",
					Sidecar:    "sidecar",
				},
				GCSConfiguration: &GCSConfiguration{
					Bucket:       "bucket",
					PathPrefix:   "prefix",
					PathStrategy: PathStrategyLegacy,
					DefaultOrg:   "org",
					DefaultRepo:  "repo",
				},
				GCSCredentialsSecret: "secretName",
				SSHKeySecrets:        []string{"first", "second"},
				SSHHostFingerprints:  []string{"primero", "segundo"},
				SkipCloning:          &truth,
			}
			t.Parallel()

			expected := tc.expected(tc.provided, defaults)
			if actual := tc.provided.ApplyDefault(defaults); !reflect.DeepEqual(actual, expected) {
				t.Errorf("expected defaulted config %v but got %v", expected, actual)
			}
		})
	}
}

func TestRefsToString(t *testing.T) {
	var tests = []struct {
		name     string
		ref      Refs
		expected string
	}{
		{
			name: "Refs with Pull",
			ref: Refs{
				BaseRef: "master",
				BaseSHA: "deadbeef",
				Pulls: []Pull{
					{
						Number: 123,
						SHA:    "abcd1234",
					},
				},
			},
			expected: "master:deadbeef,123:abcd1234",
		},
		{
			name: "Refs with multiple Pulls",
			ref: Refs{
				BaseRef: "master",
				BaseSHA: "deadbeef",
				Pulls: []Pull{
					{
						Number: 123,
						SHA:    "abcd1234",
					},
					{
						Number: 456,
						SHA:    "dcba4321",
					},
				},
			},
			expected: "master:deadbeef,123:abcd1234,456:dcba4321",
		},
		{
			name: "Refs with BaseRef only",
			ref: Refs{
				BaseRef: "master",
			},
			expected: "master",
		},
		{
			name: "Refs with BaseRef and BaseSHA",
			ref: Refs{
				BaseRef: "master",
				BaseSHA: "deadbeef",
			},
			expected: "master:deadbeef",
		},
	}

	for _, test := range tests {
		actual, expected := test.ref.String(), test.expected
		if actual != expected {
			t.Errorf("%s: got ref string: %s, but expected: %s", test.name, actual, expected)
		}
	}
}

func TestRerunAuthConfigValidate(t *testing.T) {
	var testCases = []struct {
		name        string
		config      *RerunAuthConfig
		errExpected bool
	}{
		{
			name:        "disallow all",
			config:      &RerunAuthConfig{AllowAnyone: false},
			errExpected: false,
		},
		{
			name:        "no restrictions",
			config:      &RerunAuthConfig{},
			errExpected: false,
		},
		{
			name:        "allow any",
			config:      &RerunAuthConfig{AllowAnyone: true},
			errExpected: false,
		},
		{
			name:        "restrict orgs",
			config:      &RerunAuthConfig{GitHubOrgs: []string{"istio"}},
			errExpected: false,
		},
		{
			name:        "restrict orgs and users",
			config:      &RerunAuthConfig{GitHubOrgs: []string{"istio", "kubernetes"}, GitHubUsers: []string{"clarketm", "scoobydoo"}},
			errExpected: false,
		},
		{
			name:        "allow any and has restriction",
			config:      &RerunAuthConfig{AllowAnyone: true, GitHubOrgs: []string{"istio"}},
			errExpected: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			if err := tc.config.Validate(); (err != nil) != tc.errExpected {
				t.Errorf("Expected error %v, got %v", tc.errExpected, err)
			}
		})
	}
}

func TestRerunAuthConfigsGetRerunAuthConfig(t *testing.T) {
	var testCases = []struct {
		name     string
		configs  RerunAuthConfigs
		refs     *Refs
		expected RerunAuthConfig
	}{
		{
			name:     "default to an empty config",
			configs:  RerunAuthConfigs{},
			refs:     &Refs{Org: "my-default-org", Repo: "my-default-repo"},
			expected: RerunAuthConfig{},
		},
		{
			name:     "unknown org or org/repo return wildcard",
			configs:  RerunAuthConfigs{"*": RerunAuthConfig{GitHubUsers: []string{"clarketm"}}},
			refs:     &Refs{Org: "my-default-org", Repo: "my-default-repo"},
			expected: RerunAuthConfig{GitHubUsers: []string{"clarketm"}},
		},
		{
			name:     "no refs return wildcard",
			configs:  RerunAuthConfigs{"*": RerunAuthConfig{GitHubUsers: []string{"leonardo"}}},
			refs:     nil,
			expected: RerunAuthConfig{GitHubUsers: []string{"leonardo"}},
		},
		{
			name: "use org if defined",
			configs: RerunAuthConfigs{
				"*":                RerunAuthConfig{GitHubUsers: []string{"clarketm"}},
				"istio":            RerunAuthConfig{GitHubUsers: []string{"scoobydoo"}},
				"istio/test-infra": RerunAuthConfig{GitHubUsers: []string{"billybob"}},
			},
			refs:     &Refs{Org: "istio", Repo: "istio"},
			expected: RerunAuthConfig{GitHubUsers: []string{"scoobydoo"}},
		},
		{
			name: "use org/repo if defined",
			configs: RerunAuthConfigs{
				"*":           RerunAuthConfig{GitHubUsers: []string{"clarketm"}},
				"istio/istio": RerunAuthConfig{GitHubUsers: []string{"skywalker"}},
			},
			refs:     &Refs{Org: "istio", Repo: "istio"},
			expected: RerunAuthConfig{GitHubUsers: []string{"skywalker"}},
		},
		{
			name: "org/repo takes precedence over org",
			configs: RerunAuthConfigs{
				"*":           RerunAuthConfig{GitHubUsers: []string{"clarketm"}},
				"istio":       RerunAuthConfig{GitHubUsers: []string{"scrappydoo"}},
				"istio/istio": RerunAuthConfig{GitHubUsers: []string{"airbender"}},
			},
			refs:     &Refs{Org: "istio", Repo: "istio"},
			expected: RerunAuthConfig{GitHubUsers: []string{"airbender"}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			if actual := tc.configs.GetRerunAuthConfig(tc.refs); !reflect.DeepEqual(actual, tc.expected) {
				t.Errorf("Expected %v, got %v", tc.expected, actual)
			}
		})
	}
}
