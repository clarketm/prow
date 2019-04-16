/*
Copyright 2016 The Kubernetes Authors.

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
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	prowapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/client/clientset/versioned/fake"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/tide"
	"k8s.io/test-infra/prow/tide/history"
)

func TestOptions_Validate(t *testing.T) {
	var testCases = []struct {
		name        string
		input       options
		expectedErr bool
	}{
		{
			name: "minimal set ok",
			input: options{
				configPath: "test",
			},
			expectedErr: false,
		},
		{
			name:        "missing configpath",
			input:       options{},
			expectedErr: true,
		},
		{
			name: "ok with oauth",
			input: options{
				configPath:            "test",
				oauthURL:              "website",
				githubOAuthConfigFile: "something",
				cookieSecretFile:      "yum",
			},
			expectedErr: false,
		},
		{
			name: "missing github config with oauth",
			input: options{
				configPath:       "test",
				oauthURL:         "website",
				cookieSecretFile: "yum",
			},
			expectedErr: true,
		},
		{
			name: "missing cookie with oauth",
			input: options{
				configPath:            "test",
				oauthURL:              "website",
				githubOAuthConfigFile: "something",
			},
			expectedErr: true,
		},
	}

	for _, testCase := range testCases {
		err := testCase.input.Validate()
		if testCase.expectedErr && err == nil {
			t.Errorf("%s: expected an error but got none", testCase.name)
		}
		if !testCase.expectedErr && err != nil {
			t.Errorf("%s: expected no error but got one: %v", testCase.name, err)
		}
	}
}

type flc int

func (f flc) GetJobLog(job, id string) ([]byte, error) {
	if job == "job" && id == "123" {
		return []byte("hello"), nil
	}
	return nil, errors.New("muahaha")
}

func TestHandleLog(t *testing.T) {
	var testcases = []struct {
		name string
		path string
		code int
	}{
		{
			name: "no job name",
			path: "",
			code: http.StatusBadRequest,
		},
		{
			name: "job but no id",
			path: "?job=job",
			code: http.StatusBadRequest,
		},
		{
			name: "id but no job",
			path: "?id=123",
			code: http.StatusBadRequest,
		},
		{
			name: "id and job, found",
			path: "?job=job&id=123",
			code: http.StatusOK,
		},
		{
			name: "id and job, not found",
			path: "?job=ohno&id=123",
			code: http.StatusNotFound,
		},
	}
	handler := handleLog(flc(0))
	for _, tc := range testcases {
		req, err := http.NewRequest(http.MethodGet, "", nil)
		if err != nil {
			t.Fatalf("Error making request: %v", err)
		}
		u, err := url.Parse(tc.path)
		if err != nil {
			t.Fatalf("Error parsing URL: %v", err)
		}
		var follow = false
		if ok, _ := strconv.ParseBool(u.Query().Get("follow")); ok {
			follow = true
		}
		req.URL = u
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != tc.code {
			t.Errorf("Wrong error code. Got %v, want %v", rr.Code, tc.code)
		} else if rr.Code == http.StatusOK {
			if follow {
				//wait a little to get the chunks
				time.Sleep(2 * time.Millisecond)
				reader := bufio.NewReader(rr.Body)
				var buf bytes.Buffer
				for {
					line, err := reader.ReadBytes('\n')
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatalf("Expecting reply with content but got error: %v", err)
					}
					buf.Write(line)
				}
				if !bytes.Contains(buf.Bytes(), []byte("hello")) {
					t.Errorf("Unexpected body: got %s.", buf.String())
				}
			} else {
				resp := rr.Result()
				defer resp.Body.Close()
				if body, err := ioutil.ReadAll(resp.Body); err != nil {
					t.Errorf("Error reading response body: %v", err)
				} else if string(body) != "hello" {
					t.Errorf("Unexpected body: got %s.", string(body))
				}
			}
		}
	}
}

// TestRerun just checks that the result can be unmarshaled properly, has an
// updated status, and has equal spec.
func TestRerun(t *testing.T) {
	fakeProwJobClient := fake.NewSimpleClientset(&prowapi.ProwJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wowsuch",
			Namespace: "prowjobs",
		},
		Spec: prowapi.ProwJobSpec{
			Job:  "whoa",
			Type: prowapi.PresubmitJob,
			Refs: &prowapi.Refs{
				Org:  "org",
				Repo: "repo",
				Pulls: []prowapi.Pull{
					{Number: 1},
				},
			},
		},
		Status: prowapi.ProwJobStatus{
			State: prowapi.PendingState,
		},
	})
	handler := handleRerun(fakeProwJobClient.ProwV1().ProwJobs("prowjobs"))
	req, err := http.NewRequest(http.MethodGet, "/rerun?prowjob=wowsuch", nil)
	if err != nil {
		t.Fatalf("Error making request: %v", err)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("Bad error code: %d", rr.Code)
	}
	resp := rr.Result()
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Error reading response body: %v", err)
	}
	var res prowapi.ProwJob
	if err := yaml.Unmarshal(body, &res); err != nil {
		t.Fatalf("Error unmarshaling: %v", err)
	}
	if res.Spec.Job != "whoa" {
		t.Errorf("Wrong job, expected \"whoa\", got \"%s\"", res.Spec.Job)
	}
	if res.Status.State != prowapi.TriggeredState {
		t.Errorf("Wrong state, expected \"%v\", got \"%v\"", prowapi.TriggeredState, res.Status.State)
	}
}

func TestTide(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pools := []tide.Pool{
			{
				Org: "o",
			},
		}
		b, err := json.Marshal(pools)
		if err != nil {
			t.Fatalf("Marshaling: %v", err)
		}
		fmt.Fprintf(w, string(b))
	}))
	ca := &config.Agent{}
	ca.Set(&config.Config{
		ProwConfig: config.ProwConfig{
			Tide: config.Tide{
				Queries: []config.TideQuery{
					{Repos: []string{"prowapi.netes/test-infra"}},
				},
			},
		},
	})
	ta := tideAgent{
		path:         s.URL,
		updatePeriod: func() time.Duration { return time.Minute },
	}
	if err := ta.updatePools(); err != nil {
		t.Fatalf("Updating: %v", err)
	}
	if len(ta.pools) != 1 {
		t.Fatalf("Wrong number of pools. Got %d, expected 1 in %v", len(ta.pools), ta.pools)
	}
	if ta.pools[0].Org != "o" {
		t.Errorf("Wrong org in pool. Got %s, expected o in %v", ta.pools[0].Org, ta.pools)
	}
	handler := handleTidePools(ca.Config, &ta)
	req, err := http.NewRequest(http.MethodGet, "/tide.js", nil)
	if err != nil {
		t.Fatalf("Error making request: %v", err)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("Bad error code: %d", rr.Code)
	}
	resp := rr.Result()
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Error reading response body: %v", err)
	}
	res := tidePools{}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("Error unmarshaling: %v", err)
	}
	if len(res.Pools) != 1 {
		t.Fatalf("Wrong number of pools. Got %d, expected 1 in %v", len(res.Pools), res.Pools)
	}
	if res.Pools[0].Org != "o" {
		t.Errorf("Wrong org in pool. Got %s, expected o in %v", res.Pools[0].Org, res.Pools)
	}
	if len(res.Queries) != 1 {
		t.Fatalf("Wrong number of pools. Got %d, expected 1 in %v", len(res.Queries), res.Queries)
	}
	if expected := "is:pr state:open repo:\"prowapi.netes/test-infra\""; res.Queries[0] != expected {
		t.Errorf("Wrong query. Got %s, expected %s", res.Queries[0], expected)
	}
}

func TestTideHistory(t *testing.T) {
	testHist := map[string][]history.Record{
		"o/r:b": {
			{Action: "MERGE"}, {Action: "TRIGGER"},
		},
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := json.Marshal(testHist)
		if err != nil {
			t.Fatalf("Marshaling: %v", err)
		}
		fmt.Fprintf(w, string(b))
	}))

	ta := tideAgent{
		path:         s.URL,
		updatePeriod: func() time.Duration { return time.Minute },
	}
	if err := ta.updateHistory(); err != nil {
		t.Fatalf("Updating: %v", err)
	}
	if !reflect.DeepEqual(ta.history, testHist) {
		t.Fatalf("Expected tideAgent history:\n%#v\n,but got:\n%#v\n", testHist, ta.history)
	}

	handler := handleTideHistory(&ta)
	req, err := http.NewRequest(http.MethodGet, "/tide-history.js", nil)
	if err != nil {
		t.Fatalf("Error making request: %v", err)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("Bad error code: %d", rr.Code)
	}
	resp := rr.Result()
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Error reading response body: %v", err)
	}
	var res tideHistory
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("Error unmarshaling: %v", err)
	}
	if !reflect.DeepEqual(res.History, testHist) {
		t.Fatalf("Expected /tide-history.js:\n%#v\n,but got:\n%#v\n", testHist, res.History)
	}
}

func TestHelp(t *testing.T) {
	hitCount := 0
	help := pluginhelp.Help{
		AllRepos:            []string{"org/repo"},
		RepoPlugins:         map[string][]string{"org": {"plugin"}},
		RepoExternalPlugins: map[string][]string{"org/repo": {"external-plugin"}},
		PluginHelp:          map[string]pluginhelp.PluginHelp{"plugin": {Description: "plugin"}},
		ExternalPluginHelp:  map[string]pluginhelp.PluginHelp{"external-plugin": {Description: "external-plugin"}},
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount++
		b, err := json.Marshal(help)
		if err != nil {
			t.Fatalf("Marshaling: %v", err)
		}
		fmt.Fprintf(w, string(b))
	}))
	ha := &helpAgent{
		path: s.URL,
	}
	handler := handlePluginHelp(ha)
	handleAndCheck := func() {
		req, err := http.NewRequest(http.MethodGet, "/plugin-help.js", nil)
		if err != nil {
			t.Fatalf("Error making request: %v", err)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("Bad error code: %d", rr.Code)
		}
		resp := rr.Result()
		defer resp.Body.Close()
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("Error reading response body: %v", err)
		}
		var res pluginhelp.Help
		if err := yaml.Unmarshal(body, &res); err != nil {
			t.Fatalf("Error unmarshaling: %v", err)
		}
		if !reflect.DeepEqual(help, res) {
			t.Errorf("Invalid plugin help. Got %v, expected %v", res, help)
		}
		if hitCount != 1 {
			t.Errorf("Expected fake hook endpoint to be hit once, but endpoint was hit %d times.", hitCount)
		}
	}
	handleAndCheck()
	handleAndCheck()
}

func TestListProwJobs(t *testing.T) {
	templateJob := &prowapi.ProwJob{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "prowjobs",
		},
	}

	var testCases = []struct {
		name        string
		selector    string
		prowJobs    []func(*prowapi.ProwJob) runtime.Object
		listErr     bool
		hiddenRepos sets.String
		hiddenOnly  bool
		showHidden  bool
		expected    sets.String
		expectedErr bool
	}{
		{
			name:        "list error results in filter error",
			listErr:     true,
			expectedErr: true,
		},
		{
			name:     "no hidden repos returns all prowjobs",
			selector: labels.Everything().String(),
			prowJobs: []func(*prowapi.ProwJob) runtime.Object{
				func(in *prowapi.ProwJob) runtime.Object {
					in.Name = "first"
					return in
				},
			},
			expected: sets.NewString("first"),
		},
		{
			name:     "no hidden repos returns all prowjobs except those not matching label selector",
			selector: "foo=bar",
			prowJobs: []func(*prowapi.ProwJob) runtime.Object{
				func(in *prowapi.ProwJob) runtime.Object {
					in.Name = "first"
					return in
				},
				func(in *prowapi.ProwJob) runtime.Object {
					in.Name = "second"
					in.Labels = map[string]string{"foo": "bar"}
					return in
				},
			},
			expected: sets.NewString("second"),
		},
		{
			name:     "hidden repos excludes prowjobs from those repos",
			selector: labels.Everything().String(),
			prowJobs: []func(*prowapi.ProwJob) runtime.Object{
				func(in *prowapi.ProwJob) runtime.Object {
					in.Name = "first"
					return in
				},
				func(in *prowapi.ProwJob) runtime.Object {
					in.Name = "second"
					in.Spec.Refs = &prowapi.Refs{
						Org:  "org",
						Repo: "repo",
					}
					return in
				},
			},
			hiddenRepos: sets.NewString("org/repo"),
			expected:    sets.NewString("first"),
		},
		{
			name:     "hidden repos doesn't exclude prowjobs from other repos",
			selector: labels.Everything().String(),
			prowJobs: []func(*prowapi.ProwJob) runtime.Object{
				func(in *prowapi.ProwJob) runtime.Object {
					in.Name = "first"
					return in
				},
				func(in *prowapi.ProwJob) runtime.Object {
					in.Name = "second"
					in.Spec.Refs = &prowapi.Refs{
						Org:  "org",
						Repo: "other",
					}
					return in
				},
			},
			hiddenRepos: sets.NewString("org/repo"),
			expected:    sets.NewString("first", "second"),
		},
		{
			name:     "hidden orgs excludes prowjobs from those orgs",
			selector: labels.Everything().String(),
			prowJobs: []func(*prowapi.ProwJob) runtime.Object{
				func(in *prowapi.ProwJob) runtime.Object {
					in.Name = "first"
					return in
				},
				func(in *prowapi.ProwJob) runtime.Object {
					in.Name = "second"
					in.Spec.Refs = &prowapi.Refs{
						Org:  "org",
						Repo: "other",
					}
					return in
				},
			},
			hiddenRepos: sets.NewString("org"),
			expected:    sets.NewString("first"),
		},
		{
			name:     "hidden orgs doesn't exclude prowjobs from other orgs",
			selector: labels.Everything().String(),
			prowJobs: []func(*prowapi.ProwJob) runtime.Object{
				func(in *prowapi.ProwJob) runtime.Object {
					in.Name = "first"
					return in
				},
				func(in *prowapi.ProwJob) runtime.Object {
					in.Name = "second"
					in.Spec.Refs = &prowapi.Refs{
						Org:  "other",
						Repo: "other",
					}
					return in
				},
			},
			hiddenRepos: sets.NewString("org"),
			expected:    sets.NewString("first", "second"),
		},
		{
			name:     "hidden repos excludes prowjobs from those repos even by extra_refs",
			selector: labels.Everything().String(),
			prowJobs: []func(*prowapi.ProwJob) runtime.Object{
				func(in *prowapi.ProwJob) runtime.Object {
					in.Name = "first"
					in.Spec.ExtraRefs = []prowapi.Refs{{Org: "org", Repo: "repo"}}
					return in
				},
			},
			hiddenRepos: sets.NewString("org/repo"),
			expected:    sets.NewString(),
		},
		{
			name:     "hidden orgs excludes prowjobs from those orgs even by extra_refs",
			selector: labels.Everything().String(),
			prowJobs: []func(*prowapi.ProwJob) runtime.Object{
				func(in *prowapi.ProwJob) runtime.Object {
					in.Name = "first"
					in.Spec.ExtraRefs = []prowapi.Refs{{Org: "org", Repo: "repo"}}
					return in
				},
			},
			hiddenRepos: sets.NewString("org"),
			expected:    sets.NewString(),
		},
		{
			name:     "prowjobs without refs are returned even with hidden repos filtering",
			selector: labels.Everything().String(),
			prowJobs: []func(*prowapi.ProwJob) runtime.Object{
				func(in *prowapi.ProwJob) runtime.Object {
					in.Name = "first"
					return in
				},
			},
			hiddenRepos: sets.NewString("org/repo"),
			expected:    sets.NewString("first"),
		},
		{
			name:     "all prowjobs are returned when showHidden is true",
			selector: labels.Everything().String(),
			prowJobs: []func(*prowapi.ProwJob) runtime.Object{
				func(in *prowapi.ProwJob) runtime.Object {
					in.Name = "first"
					in.Spec.ExtraRefs = []prowapi.Refs{{Org: "org", Repo: "repo"}}
					return in
				},
				func(in *prowapi.ProwJob) runtime.Object {
					in.Name = "second"
					return in
				},
			},
			hiddenRepos: sets.NewString("org/repo"),
			expected:    sets.NewString("first", "second"),
			showHidden:  true,
		},
	}

	for _, testCase := range testCases {
		var data []runtime.Object
		for _, generator := range testCase.prowJobs {
			data = append(data, generator(templateJob.DeepCopy()))
		}
		fakeProwJobClient := fake.NewSimpleClientset(data...)
		if testCase.listErr {
			fakeProwJobClient.PrependReactor("*", "*", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
				return true, nil, errors.New("could not list ProwJobs")
			})
		}
		lister := filteringProwJobLister{
			client:      fakeProwJobClient.ProwV1().ProwJobs("prowjobs"),
			hiddenRepos: testCase.hiddenRepos,
			hiddenOnly:  testCase.hiddenOnly,
			showHidden:  testCase.showHidden,
		}

		filtered, err := lister.ListProwJobs(testCase.selector)
		if err == nil && testCase.expectedErr {
			t.Errorf("%s: expected an error but got none", testCase.name)
		}
		if err != nil && !testCase.expectedErr {
			t.Errorf("%s: expected no error but got one: %v", testCase.name, err)
		}

		filteredNames := sets.NewString()
		for _, prowJob := range filtered {
			filteredNames.Insert(prowJob.Name)
		}

		if missing := testCase.expected.Difference(filteredNames); missing.Len() > 0 {
			t.Errorf("%s: did not get expected jobs in filtered list: %v", testCase.name, missing.List())
		}
		if extra := filteredNames.Difference(testCase.expected); extra.Len() > 0 {
			t.Errorf("%s: got unexpected jobs in filtered list: %v", testCase.name, extra.List())
		}
	}
}
