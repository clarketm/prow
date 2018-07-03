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

package config

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/test-infra/prow/kube"
)

func TestDecorationDefaulting(t *testing.T) {
	defaults := &kube.DecorationConfig{
		Timeout:     1 * time.Minute,
		GracePeriod: 10 * time.Second,
		UtilityImages: &kube.UtilityImages{
			CloneRefs:  "clonerefs",
			InitUpload: "iniupload",
			Entrypoint: "entrypoint",
			Sidecar:    "sidecar",
		},
		GCSConfiguration: &kube.GCSConfiguration{
			Bucket:       "bucket",
			PathPrefix:   "prefix",
			PathStrategy: kube.PathStrategyLegacy,
			DefaultOrg:   "org",
			DefaultRepo:  "repo",
		},
		GCSCredentialsSecret: "secretName",
		SSHKeySecrets:        []string{"first", "second"},
	}

	var testCases = []struct {
		name     string
		provided *kube.DecorationConfig
		expected *kube.DecorationConfig
	}{
		{
			name:     "nothing provided",
			provided: &kube.DecorationConfig{},
			expected: &kube.DecorationConfig{
				Timeout:     1 * time.Minute,
				GracePeriod: 10 * time.Second,
				UtilityImages: &kube.UtilityImages{
					CloneRefs:  "clonerefs",
					InitUpload: "iniupload",
					Entrypoint: "entrypoint",
					Sidecar:    "sidecar",
				},
				GCSConfiguration: &kube.GCSConfiguration{
					Bucket:       "bucket",
					PathPrefix:   "prefix",
					PathStrategy: kube.PathStrategyLegacy,
					DefaultOrg:   "org",
					DefaultRepo:  "repo",
				},
				GCSCredentialsSecret: "secretName",
				SSHKeySecrets:        []string{"first", "second"},
			},
		},
		{
			name: "timeout provided",
			provided: &kube.DecorationConfig{
				Timeout: 10 * time.Minute,
			},
			expected: &kube.DecorationConfig{
				Timeout:     10 * time.Minute,
				GracePeriod: 10 * time.Second,
				UtilityImages: &kube.UtilityImages{
					CloneRefs:  "clonerefs",
					InitUpload: "iniupload",
					Entrypoint: "entrypoint",
					Sidecar:    "sidecar",
				},
				GCSConfiguration: &kube.GCSConfiguration{
					Bucket:       "bucket",
					PathPrefix:   "prefix",
					PathStrategy: kube.PathStrategyLegacy,
					DefaultOrg:   "org",
					DefaultRepo:  "repo",
				},
				GCSCredentialsSecret: "secretName",
				SSHKeySecrets:        []string{"first", "second"},
			},
		},
		{
			name: "grace period provided",
			provided: &kube.DecorationConfig{
				GracePeriod: 10 * time.Hour,
			},
			expected: &kube.DecorationConfig{
				Timeout:     1 * time.Minute,
				GracePeriod: 10 * time.Hour,
				UtilityImages: &kube.UtilityImages{
					CloneRefs:  "clonerefs",
					InitUpload: "iniupload",
					Entrypoint: "entrypoint",
					Sidecar:    "sidecar",
				},
				GCSConfiguration: &kube.GCSConfiguration{
					Bucket:       "bucket",
					PathPrefix:   "prefix",
					PathStrategy: kube.PathStrategyLegacy,
					DefaultOrg:   "org",
					DefaultRepo:  "repo",
				},
				GCSCredentialsSecret: "secretName",
				SSHKeySecrets:        []string{"first", "second"},
			},
		},
		{
			name: "utility images provided",
			provided: &kube.DecorationConfig{
				UtilityImages: &kube.UtilityImages{
					CloneRefs:  "clonerefs-special",
					InitUpload: "iniupload-special",
					Entrypoint: "entrypoint-special",
					Sidecar:    "sidecar-special",
				},
			},
			expected: &kube.DecorationConfig{
				Timeout:     1 * time.Minute,
				GracePeriod: 10 * time.Second,
				UtilityImages: &kube.UtilityImages{
					CloneRefs:  "clonerefs-special",
					InitUpload: "iniupload-special",
					Entrypoint: "entrypoint-special",
					Sidecar:    "sidecar-special",
				},
				GCSConfiguration: &kube.GCSConfiguration{
					Bucket:       "bucket",
					PathPrefix:   "prefix",
					PathStrategy: kube.PathStrategyLegacy,
					DefaultOrg:   "org",
					DefaultRepo:  "repo",
				},
				GCSCredentialsSecret: "secretName",
				SSHKeySecrets:        []string{"first", "second"},
			},
		},
		{
			name: "gcs configuration provided",
			provided: &kube.DecorationConfig{
				GCSConfiguration: &kube.GCSConfiguration{
					Bucket:       "bucket-1",
					PathPrefix:   "prefix-2",
					PathStrategy: kube.PathStrategyExplicit,
				},
			},
			expected: &kube.DecorationConfig{
				Timeout:     1 * time.Minute,
				GracePeriod: 10 * time.Second,
				UtilityImages: &kube.UtilityImages{
					CloneRefs:  "clonerefs",
					InitUpload: "iniupload",
					Entrypoint: "entrypoint",
					Sidecar:    "sidecar",
				},
				GCSConfiguration: &kube.GCSConfiguration{
					Bucket:       "bucket-1",
					PathPrefix:   "prefix-2",
					PathStrategy: kube.PathStrategyExplicit,
				},
				GCSCredentialsSecret: "secretName",
				SSHKeySecrets:        []string{"first", "second"},
			},
		},
		{
			name: "secret name provided",
			provided: &kube.DecorationConfig{
				GCSCredentialsSecret: "somethingSecret",
			},
			expected: &kube.DecorationConfig{
				Timeout:     1 * time.Minute,
				GracePeriod: 10 * time.Second,
				UtilityImages: &kube.UtilityImages{
					CloneRefs:  "clonerefs",
					InitUpload: "iniupload",
					Entrypoint: "entrypoint",
					Sidecar:    "sidecar",
				},
				GCSConfiguration: &kube.GCSConfiguration{
					Bucket:       "bucket",
					PathPrefix:   "prefix",
					PathStrategy: kube.PathStrategyLegacy,
					DefaultOrg:   "org",
					DefaultRepo:  "repo",
				},
				GCSCredentialsSecret: "somethingSecret",
				SSHKeySecrets:        []string{"first", "second"},
			},
		},
		{
			name: "ssh secrets provided",
			provided: &kube.DecorationConfig{
				SSHKeySecrets: []string{"my", "special"},
			},
			expected: &kube.DecorationConfig{
				Timeout:     1 * time.Minute,
				GracePeriod: 10 * time.Second,
				UtilityImages: &kube.UtilityImages{
					CloneRefs:  "clonerefs",
					InitUpload: "iniupload",
					Entrypoint: "entrypoint",
					Sidecar:    "sidecar",
				},
				GCSConfiguration: &kube.GCSConfiguration{
					Bucket:       "bucket",
					PathPrefix:   "prefix",
					PathStrategy: kube.PathStrategyLegacy,
					DefaultOrg:   "org",
					DefaultRepo:  "repo",
				},
				GCSCredentialsSecret: "secretName",
				SSHKeySecrets:        []string{"my", "special"},
			},
		},
	}

	for _, testCase := range testCases {
		if actual, expected := setDecorationDefaults(testCase.provided, defaults), testCase.expected; !reflect.DeepEqual(actual, expected) {
			t.Errorf("%s: expected defaulted config %v but got %v", testCase.name, expected, actual)
		}
	}
}

// integration test for fake config loading
func TestValidConfigLoading(t *testing.T) {
	var testCases = []struct {
		name               string
		prowConfig         string
		jobConfigs         []string
		expectError        bool
		expectPodNameSpace string
		expectEnv          map[string][]v1.EnvVar
	}{
		{
			name:       "one config",
			prowConfig: ``,
		},
		{
			name:       "invalid periodic",
			prowConfig: ``,
			jobConfigs: []string{
				`
periodics:
- interval: 10m
  agent: kubernetes
  name: foo`,
			},
			expectError: true,
		},
		{
			name:       "one periodic",
			prowConfig: ``,
			jobConfigs: []string{
				`
periodics:
- interval: 10m
  agent: kubernetes
  name: foo
  spec:
    containers:
    - image: alpine`,
			},
		},
		{
			name:       "two periodics",
			prowConfig: ``,
			jobConfigs: []string{
				`
periodics:
- interval: 10m
  agent: kubernetes
  name: foo
  spec:
    containers:
    - image: alpine`,
				`
periodics:
- interval: 10m
  agent: kubernetes
  name: bar
  spec:
    containers:
    - image: alpine`,
			},
		},
		{
			name:       "duplicated periodics",
			prowConfig: ``,
			jobConfigs: []string{
				`
periodics:
- interval: 10m
  agent: kubernetes
  name: foo
  spec:
    containers:
    - image: alpine`,
				`
periodics:
- interval: 10m
  agent: kubernetes
  name: foo
  spec:
    containers:
    - image: alpine`,
			},
			expectError: true,
		},
		{
			name:       "one presubmit, no context",
			prowConfig: ``,
			jobConfigs: []string{
				`
presubmits:
  foo/bar:
  - agent: kubernetes
    name: presubmit-bar
    spec:
      containers:
      - image: alpine`,
			},
			expectError: true,
		},
		{
			name:       "one presubmit, ok",
			prowConfig: ``,
			jobConfigs: []string{
				`
presubmits:
  foo/bar:
  - agent: kubernetes
    name: presubmit-bar
    context: bar
    spec:
      containers:
      - image: alpine`,
			},
		},
		{
			name:       "two presubmits",
			prowConfig: ``,
			jobConfigs: []string{
				`
presubmits:
  foo/bar:
  - agent: kubernetes
    name: presubmit-bar
    context: bar
    spec:
      containers:
      - image: alpine`,
				`
presubmits:
  foo/baz:
  - agent: kubernetes
    name: presubmit-baz
    context: baz
    spec:
      containers:
      - image: alpine`,
			},
		},
		{
			name:       "dup presubmits, one file",
			prowConfig: ``,
			jobConfigs: []string{
				`
presubmits:
  foo/bar:
  - agent: kubernetes
    name: presubmit-bar
    context: bar
    spec:
      containers:
      - image: alpine
  - agent: kubernetes
    name: presubmit-bar
    context: bar
    spec:
      containers:
      - image: alpine`,
			},
		},
		{
			name:       "dup presubmits, two files",
			prowConfig: ``,
			jobConfigs: []string{
				`
presubmits:
  foo/bar:
  - interval: 10m
    agent: kubernetes
    name: presubmit-bar
    context: bar
    spec:
      containers:
      - image: alpine`,
				`
presubmits:
  foo/bar:
  - interval: 10m
    agent: kubernetes
    context: bar
    name: presubmit-bar
    spec:
      containers:
      - image: alpine`,
			},
			expectError: true,
		},
		{
			name:       "one postsubmit, ok",
			prowConfig: ``,
			jobConfigs: []string{
				`
postsubmits:
  foo/bar:
  - agent: kubernetes
    name: postsubmit-bar
    spec:
      containers:
      - image: alpine`,
			},
		},
		{
			name:       "two postsubmits",
			prowConfig: ``,
			jobConfigs: []string{
				`
postsubmits:
  foo/bar:
  - agent: kubernetes
    name: postsubmit-bar
    context: bar
    spec:
      containers:
      - image: alpine`,
				`
postsubmits:
  foo/baz:
  - agent: kubernetes
    name: postsubmit-baz
    context: baz
    spec:
      containers:
      - image: alpine`,
			},
		},
		{
			name:       "dup postsubmits, one file",
			prowConfig: ``,
			jobConfigs: []string{
				`
postsubmits:
  foo/bar:
  - agent: kubernetes
    name: postsubmit-bar
    context: bar
    spec:
      containers:
      - image: alpine
  - agent: kubernetes
    name: postsubmit-bar
    context: bar
    spec:
      containers:
      - image: alpine`,
			},
		},
		{
			name:       "dup postsubmits, two files",
			prowConfig: ``,
			jobConfigs: []string{
				`
postsubmits:
  foo/bar:
  - interval: 10m
    agent: kubernetes
    name: postsubmit-bar
    context: bar
    spec:
      containers:
      - image: alpine`,
				`
postsubmits:
  foo/bar:
  - interval: 10m
    agent: kubernetes
    context: bar
    name: postsubmit-bar
    spec:
      containers:
      - image: alpine`,
			},
			expectError: true,
		},
		{
			name: "overwrite PodNamespace",
			prowConfig: `
pod_namespace: test`,
			jobConfigs: []string{
				`
pod_namespace: debug`,
			},
			expectPodNameSpace: "test",
		},
		{
			name: "test valid presets in main config",
			prowConfig: `
presets:
- labels:
    preset-baz: "true"
  env:
  - name: baz
    value: fejtaverse`,
			jobConfigs: []string{
				`periodics:
- interval: 10m
  agent: kubernetes
  name: foo
  labels:
    preset-baz: "true"
  spec:
    containers:
    - image: alpine`,
				`
periodics:
- interval: 10m
  agent: kubernetes
  name: bar
  labels:
    preset-baz: "true"
  spec:
    containers:
    - image: alpine`,
			},
			expectEnv: map[string][]v1.EnvVar{
				"foo": {
					{
						Name:  "baz",
						Value: "fejtaverse",
					},
				},
				"bar": {
					{
						Name:  "baz",
						Value: "fejtaverse",
					},
				},
			},
		},
		{
			name:       "test valid presets in job configs",
			prowConfig: ``,
			jobConfigs: []string{
				`
presets:
- labels:
    preset-baz: "true"
  env:
  - name: baz
    value: fejtaverse
periodics:
- interval: 10m
  agent: kubernetes
  name: foo
  labels:
    preset-baz: "true"
  spec:
    containers:
    - image: alpine`,
				`
periodics:
- interval: 10m
  agent: kubernetes
  name: bar
  labels:
    preset-baz: "true"
  spec:
    containers:
    - image: alpine`,
			},
			expectEnv: map[string][]v1.EnvVar{
				"foo": {
					{
						Name:  "baz",
						Value: "fejtaverse",
					},
				},
				"bar": {
					{
						Name:  "baz",
						Value: "fejtaverse",
					},
				},
			},
		},
		{
			name: "test valid presets in both main & job configs",
			prowConfig: `
presets:
- labels:
    preset-baz: "true"
  env:
  - name: baz
    value: fejtaverse`,
			jobConfigs: []string{
				`
presets:
- labels:
    preset-k8s: "true"
  env:
  - name: k8s
    value: kubernetes
periodics:
- interval: 10m
  agent: kubernetes
  name: foo
  labels:
    preset-baz: "true"
    preset-k8s: "true"
  spec:
    containers:
    - image: alpine`,
				`
periodics:
- interval: 10m
  agent: kubernetes
  name: bar
  labels:
    preset-baz: "true"
  spec:
    containers:
    - image: alpine`,
			},
			expectEnv: map[string][]v1.EnvVar{
				"foo": {
					{
						Name:  "baz",
						Value: "fejtaverse",
					},
					{
						Name:  "k8s",
						Value: "kubernetes",
					},
				},
				"bar": {
					{
						Name:  "baz",
						Value: "fejtaverse",
					},
				},
			},
		},
	}

	for _, tc := range testCases {
		// save the config
		prowConfigDir, err := ioutil.TempDir("", "prowConfig")
		if err != nil {
			t.Fatalf("fail to make tempdir: %v", err)
		}
		defer os.RemoveAll(prowConfigDir)

		prowConfig := filepath.Join(prowConfigDir, "config.yaml")
		if err := ioutil.WriteFile(prowConfig, []byte(tc.prowConfig), 0666); err != nil {
			t.Fatalf("fail to write prow config: %v", err)
		}

		jobConfig := ""
		if len(tc.jobConfigs) > 0 {
			jobConfigDir, err := ioutil.TempDir("", "jobConfig")
			if err != nil {
				t.Fatalf("fail to make tempdir: %v", err)
			}
			defer os.RemoveAll(jobConfigDir)

			// cover both job config as a file & a dir
			if len(tc.jobConfigs) == 1 {
				// a single file
				jobConfig = filepath.Join(jobConfigDir, "config.yaml")
				if err := ioutil.WriteFile(jobConfig, []byte(tc.jobConfigs[0]), 0666); err != nil {
					t.Fatalf("fail to write job config: %v", err)
				}
			} else {
				// a dir
				jobConfig = jobConfigDir
				for idx, config := range tc.jobConfigs {
					subConfig := filepath.Join(jobConfigDir, fmt.Sprintf("config_%d.yaml", idx))
					if err := ioutil.WriteFile(subConfig, []byte(config), 0666); err != nil {
						t.Fatalf("fail to write job config: %v", err)
					}
				}
			}
		}

		cfg, err := Load(prowConfig, jobConfig)
		if tc.expectError && err == nil {
			t.Errorf("tc %s: Expect error, but got nil", tc.name)
		} else if !tc.expectError && err != nil {
			t.Errorf("tc %s: Expect no error, but got error %v", tc.name, err)
		}

		if err == nil {
			if tc.expectPodNameSpace == "" {
				tc.expectPodNameSpace = "default"
			}

			if cfg.PodNamespace != tc.expectPodNameSpace {
				t.Errorf("tc %s: Expect PodNamespace %s, but got %v", tc.name, tc.expectPodNameSpace, cfg.PodNamespace)
			}

			if len(tc.expectEnv) > 0 {
				for _, j := range cfg.AllPresubmits(nil) {
					if envs, ok := tc.expectEnv[j.Name]; ok {
						if !reflect.DeepEqual(envs, j.Spec.Containers[0].Env) {
							t.Errorf("tc %s: expect env %v for job %s, got %+v", tc.name, envs, j.Name, j.Spec.Containers[0].Env)
						}
					}
				}

				for _, j := range cfg.AllPostsubmits(nil) {
					if envs, ok := tc.expectEnv[j.Name]; ok {
						if !reflect.DeepEqual(envs, j.Spec.Containers[0].Env) {
							t.Errorf("tc %s: expect env %v for job %s, got %+v", tc.name, envs, j.Name, j.Spec.Containers[0].Env)
						}
					}
				}

				for _, j := range cfg.AllPeriodics() {
					if envs, ok := tc.expectEnv[j.Name]; ok {
						if !reflect.DeepEqual(envs, j.Spec.Containers[0].Env) {
							t.Errorf("tc %s: expect env %v for job %s, got %+v", tc.name, envs, j.Name, j.Spec.Containers[0].Env)
						}
					}
				}
			}
		}
	}
}
