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

package main

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	prowapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/client/clientset/versioned/fake"
	"k8s.io/test-infra/prow/config"
)

type fakeCron struct {
	jobs []string
}

func (fc *fakeCron) SyncConfig(cfg *config.Config) error {
	for _, p := range cfg.Periodics {
		if p.Cron != "" {
			fc.jobs = append(fc.jobs, p.Name)
		}
	}

	return nil
}

func (fc *fakeCron) QueuedJobs() []string {
	res := []string{}
	for _, job := range fc.jobs {
		res = append(res, job)
	}
	fc.jobs = []string{}
	return res
}

// Assumes there is one periodic job called "p" with an interval of one minute.
func TestSync(t *testing.T) {
	testcases := []struct {
		testName string

		jobName         string
		jobComplete     bool
		jobStartTimeAgo time.Duration

		shouldStart bool
	}{
		{
			testName:    "no job",
			shouldStart: true,
		},
		{
			testName:        "job with other name",
			jobName:         "not-j",
			jobComplete:     true,
			jobStartTimeAgo: time.Hour,
			shouldStart:     true,
		},
		{
			testName:        "old, complete job",
			jobName:         "j",
			jobComplete:     true,
			jobStartTimeAgo: time.Hour,
			shouldStart:     true,
		},
		{
			testName:        "old, incomplete job",
			jobName:         "j",
			jobComplete:     false,
			jobStartTimeAgo: time.Hour,
			shouldStart:     false,
		},
		{
			testName:        "new, complete job",
			jobName:         "j",
			jobComplete:     true,
			jobStartTimeAgo: time.Second,
			shouldStart:     false,
		},
		{
			testName:        "new, incomplete job",
			jobName:         "j",
			jobComplete:     false,
			jobStartTimeAgo: time.Second,
			shouldStart:     false,
		},
	}
	for _, tc := range testcases {
		cfg := config.Config{
			ProwConfig: config.ProwConfig{
				ProwJobNamespace: "prowjobs",
			},
			JobConfig: config.JobConfig{
				Periodics: []config.Periodic{{JobBase: config.JobBase{Name: "j"}}},
			},
		}
		cfg.Periodics[0].SetInterval(time.Minute)

		var jobs []runtime.Object
		now := time.Now()
		if tc.jobName != "" {
			job := &prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "with-interval",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Type: prowapi.PeriodicJob,
					Job:  tc.jobName,
				},
				Status: prowapi.ProwJobStatus{
					StartTime: metav1.NewTime(now.Add(-tc.jobStartTimeAgo)),
				},
			}
			complete := metav1.NewTime(now.Add(-time.Millisecond))
			if tc.jobComplete {
				job.Status.CompletionTime = &complete
			}
			jobs = append(jobs, job)
		}
		fakeProwJobClient := fake.NewSimpleClientset(jobs...)
		fc := &fakeCron{}
		if err := sync(fakeProwJobClient.ProwV1().ProwJobs(cfg.ProwJobNamespace), &cfg, fc, now); err != nil {
			t.Fatalf("For case %s, didn't expect error: %v", tc.testName, err)
		}

		sawCreation := false
		for _, action := range fakeProwJobClient.Fake.Actions() {
			switch action.(type) {
			case clienttesting.CreateActionImpl:
				sawCreation = true
			}
		}
		if tc.shouldStart != sawCreation {
			t.Errorf("For case %s, did the wrong thing.", tc.testName)
		}
	}
}

// Test sync periodic job scheduled by cron.
func TestSyncCron(t *testing.T) {
	testcases := []struct {
		testName    string
		jobName     string
		jobComplete bool
		shouldStart bool
	}{
		{
			testName:    "no job",
			shouldStart: true,
		},
		{
			testName:    "job with other name",
			jobName:     "not-j",
			jobComplete: true,
			shouldStart: true,
		},
		{
			testName:    "job still running",
			jobName:     "j",
			jobComplete: false,
			shouldStart: false,
		},
		{
			testName:    "job finished",
			jobName:     "j",
			jobComplete: true,
			shouldStart: true,
		},
	}
	for _, tc := range testcases {
		cfg := config.Config{
			ProwConfig: config.ProwConfig{
				ProwJobNamespace: "prowjobs",
			},
			JobConfig: config.JobConfig{
				Periodics: []config.Periodic{{JobBase: config.JobBase{Name: "j"}, Cron: "@every 1m"}},
			},
		}

		var jobs []runtime.Object
		now := time.Now()
		if tc.jobName != "" {
			job := &prowapi.ProwJob{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "with-cron",
					Namespace: "prowjobs",
				},
				Spec: prowapi.ProwJobSpec{
					Type: prowapi.PeriodicJob,
					Job:  tc.jobName,
				},
				Status: prowapi.ProwJobStatus{
					StartTime: metav1.NewTime(now.Add(-time.Hour)),
				},
			}
			complete := metav1.NewTime(now.Add(-time.Millisecond))
			if tc.jobComplete {
				job.Status.CompletionTime = &complete
			}
			jobs = append(jobs, job)
		}
		fakeProwJobClient := fake.NewSimpleClientset(jobs...)
		fc := &fakeCron{}
		if err := sync(fakeProwJobClient.ProwV1().ProwJobs(cfg.ProwJobNamespace), &cfg, fc, now); err != nil {
			t.Fatalf("For case %s, didn't expect error: %v", tc.testName, err)
		}

		sawCreation := false
		for _, action := range fakeProwJobClient.Fake.Actions() {
			switch action.(type) {
			case clienttesting.CreateActionImpl:
				sawCreation = true
			}
		}
		if tc.shouldStart != sawCreation {
			t.Errorf("For case %s, did the wrong thing.", tc.testName)
		}
	}
}
