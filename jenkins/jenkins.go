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

package jenkins

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/pjutil"
)

const (
	maxRetries = 5
	retryDelay = 100 * time.Millisecond
	buildID    = "buildId"
	newBuildID = "BUILD_ID"
)

const (
	Succeess = "SUCCESS"
	Failure  = "FAILURE"
	Aborted  = "ABORTED"
)

// NotFoundError is returned by the Jenkins client when
// a job does not exist in Jenkins.
type NotFoundError struct {
	e error
}

func (e NotFoundError) Error() string {
	return e.e.Error()
}

// NewNotFoundError creates a new NotFoundError.
func NewNotFoundError(e error) NotFoundError {
	return NotFoundError{e: e}
}

type Action struct {
	Parameters []Parameter `json:"parameters"`
}

type Parameter struct {
	Name string `json:"name"`
	// This needs to be an interface so we won't clobber
	// json unmarshaling when the Jenkins job has more
	// parameter types than strings.
	Value interface{} `json:"value"`
}

type JenkinsBuild struct {
	Actions []Action `json:"actions"`
	Task    struct {
		// Used for tracking unscheduled builds for jobs.
		Name string `json:"name"`
	} `json:"task"`
	Number   int     `json:"number"`
	Result   *string `json:"result"`
	enqueued bool
}

func (jb *JenkinsBuild) IsRunning() bool {
	return jb.Result == nil
}

func (jb *JenkinsBuild) IsSuccess() bool {
	return jb.Result != nil && *jb.Result == Succeess
}

func (jb *JenkinsBuild) IsFailure() bool {
	return jb.Result != nil && *jb.Result == Failure
}

func (jb *JenkinsBuild) IsAborted() bool {
	return jb.Result != nil && *jb.Result == Aborted
}

func (jb *JenkinsBuild) IsEnqueued() bool {
	return jb.enqueued
}

func (jb *JenkinsBuild) BuildID() string {
	for _, action := range jb.Actions {
		for _, p := range action.Parameters {
			if p.Name == buildID || p.Name == newBuildID {
				value, ok := p.Value.(string)
				if !ok {
					logrus.Errorf("Cannot determine %s value for %#v", p.Name, jb)
					continue
				}
				return value
			}
		}
	}
	return ""
}

type Client struct {
	// If logger is non-nil, log all method calls with it.
	logger *logrus.Entry

	client     *http.Client
	baseURL    string
	authConfig *AuthConfig

	metrics *ClientMetrics
}

// AuthConfig configures how we auth with Jenkins.
// Only one of the fields will be non-nil.
type AuthConfig struct {
	Basic       *BasicAuthConfig
	BearerToken *BearerTokenAuthConfig
}

type BasicAuthConfig struct {
	User  string
	Token string
}

type BearerTokenAuthConfig struct {
	Token string
}

func NewClient(url string, authConfig *AuthConfig, logger *logrus.Entry, metrics *ClientMetrics) *Client {
	if logger == nil {
		logger = logrus.NewEntry(logrus.StandardLogger())
	}
	return &Client{
		logger:     logger.WithField("client", "jenkins"),
		baseURL:    url,
		authConfig: authConfig,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		metrics: metrics,
	}
}

// measure records metrics about the provided method, path, and code.
// start needs to be recorded before doing the request.
func (c *Client) measure(method, path string, code int, start time.Time) {
	if c.metrics == nil {
		return
	}
	c.metrics.RequestLatency.WithLabelValues(method, path).Observe(time.Since(start).Seconds())
	c.metrics.Requests.WithLabelValues(method, path, fmt.Sprintf("%d", code)).Inc()
}

// GetSkipMetrics fetches the data found in the provided path. It returns the
// content of the response or any errors that occurred during the request or
// http errors. Metrics will not be gathered for this request.
func (c *Client) GetSkipMetrics(path string) ([]byte, error) {
	resp, err := c.request(http.MethodGet, path, nil, false)
	if err != nil {
		return nil, err
	}
	return readResp(resp)
}

// Get fetches the data found in the provided path. It returns the
// content of the response or any errors that occurred during the
// request or http errors.
func (c *Client) Get(path string) ([]byte, error) {
	resp, err := c.request(http.MethodGet, path, nil, true)
	if err != nil {
		return nil, err
	}
	return readResp(resp)
}

func readResp(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, NewNotFoundError(errors.New(resp.Status))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("response not 2XX: %s", resp.Status)
	}
	buf, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// request executes a request with the provided method and path.
// It retries on transport failures and 500s. measure is provided
// to enable or disable gathering metrics for specific requests
// to avoid high-cardinality metrics.
func (c *Client) request(method, path string, params url.Values, measure bool) (*http.Response, error) {
	var resp *http.Response
	var err error
	backoff := retryDelay

	urlPath := fmt.Sprintf("%s%s", c.baseURL, path)
	if params != nil {
		urlPath = fmt.Sprintf("%s?%s", urlPath, params.Encode())
	}

	start := time.Now()
	for retries := 0; retries < maxRetries; retries++ {
		resp, err = c.doRequest(method, urlPath)
		if err == nil && resp.StatusCode < 500 {
			break
		} else if err == nil && retries+1 < maxRetries {
			resp.Body.Close()
		}
		// Capture the retry in a metric.
		if measure && c.metrics != nil {
			c.metrics.RequestRetries.Inc()
		}
		time.Sleep(backoff)
		backoff *= 2
	}
	if measure && resp != nil {
		c.measure(method, path, resp.StatusCode, start)
	}
	return resp, err
}

// doRequest executes a request with the provided method and path
// exactly once. It sets up authentication if the jenkins client
// is configured accordingly. It's up to callers of this function
// to build retries and error handling.
func (c *Client) doRequest(method, path string) (*http.Response, error) {
	req, err := http.NewRequest(method, path, nil)
	if err != nil {
		return nil, err
	}
	if c.authConfig != nil {
		if c.authConfig.Basic != nil {
			req.SetBasicAuth(c.authConfig.Basic.User, c.authConfig.Basic.Token)
		}
		if c.authConfig.BearerToken != nil {
			req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.authConfig.BearerToken.Token))
		}
	}
	return c.client.Do(req)
}

// Build triggers a Jenkins build for the provided ProwJob. The name of
// the ProwJob is going to be used as the buildId parameter that will help
// us track the build before it's scheduled by Jenkins.
func (c *Client) Build(pj *kube.ProwJob) error {
	c.logger.WithFields(pjutil.ProwJobFields(pj)).Info("Build")
	return c.BuildFromSpec(&pj.Spec, pj.Metadata.Name)
}

// BuildFromSpec triggers a Jenkins build for the provided ProwJobSpec.
// buildId helps us track the build before it's scheduled by Jenkins.
func (c *Client) BuildFromSpec(spec *kube.ProwJobSpec, buildId string) error {
	env, err := pjutil.EnvForSpec(pjutil.NewJobSpec(*spec, buildId))
	if err != nil {
		return err
	}
	params := url.Values{}
	for key, value := range env {
		params.Set(key, value)
	}
	path := fmt.Sprintf("/job/%s/buildWithParameters", spec.Job)
	resp, err := c.request(http.MethodPost, path, params, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		return fmt.Errorf("response not 201: %s", resp.Status)
	}
	return nil
}

// ListBuilds returns a list of all Jenkins builds for the
// provided jobs (both scheduled and enqueued).
func (c *Client) ListBuilds(jobs []string) (map[string]JenkinsBuild, error) {
	// Get queued builds.
	jenkinsBuilds, err := c.GetEnqueuedBuilds(jobs)
	if err != nil {
		return nil, err
	}

	buildChan := make(chan map[string]JenkinsBuild, len(jobs))
	errChan := make(chan error, len(jobs))
	wg := &sync.WaitGroup{}
	wg.Add(len(jobs))

	// Get all running builds for all provided jobs.
	for _, job := range jobs {
		// Start a goroutine per list
		go func(job string) {
			defer wg.Done()

			builds, err := c.GetBuilds(job)
			if err != nil {
				errChan <- err
			} else {
				buildChan <- builds
			}
		}(job)
	}
	wg.Wait()

	close(buildChan)
	close(errChan)

	for err := range errChan {
		if err != nil {
			return nil, err
		}
	}

	for builds := range buildChan {
		for id, build := range builds {
			jenkinsBuilds[id] = build
		}
	}

	return jenkinsBuilds, nil
}

// GetEnqueuedBuilds lists all enqueued builds for the provided jobs.
func (c *Client) GetEnqueuedBuilds(jobs []string) (map[string]JenkinsBuild, error) {
	c.logger.Debug("GetEnqueuedBuilds")

	data, err := c.Get("/queue/api/json?tree=items[task[name],actions[parameters[name,value]]]")
	if err != nil {
		return nil, fmt.Errorf("cannot list builds from the queue: %v", err)
	}
	page := struct {
		QueuedBuilds []JenkinsBuild `json:"items"`
	}{}
	if err := json.Unmarshal(data, &page); err != nil {
		return nil, fmt.Errorf("cannot unmarshal builds from the queue: %v", err)
	}
	jenkinsBuilds := make(map[string]JenkinsBuild)
	for _, jb := range page.QueuedBuilds {
		buildID := jb.BuildID()
		// Ignore builds with missing buildId parameters.
		if buildID == "" {
			continue
		}
		// Ignore builds for jobs we didn't ask for.
		var exists bool
		for _, job := range jobs {
			if jb.Task.Name == job {
				exists = true
				break
			}
		}
		if !exists {
			continue
		}
		jb.enqueued = true
		jenkinsBuilds[buildID] = jb
	}
	return jenkinsBuilds, nil
}

// GetBuilds lists all scheduled builds for the provided job.
// In newer Jenkins versions, this also includes enqueued
// builds (tested in 2.73.2).
func (c *Client) GetBuilds(job string) (map[string]JenkinsBuild, error) {
	c.logger.Debugf("GetBuilds(%v)", job)

	data, err := c.Get(fmt.Sprintf("/job/%s/api/json?tree=builds[number,result,actions[parameters[name,value]]]", job))
	if err != nil {
		// Ignore 404s so we will not block processing the rest of the jobs.
		if _, isNotFound := err.(NotFoundError); isNotFound {
			c.logger.WithError(err).Warnf("Cannot list builds for job %q", job)
			return nil, nil
		}
		return nil, fmt.Errorf("cannot list builds for job %q: %v", job, err)
	}
	page := struct {
		Builds []JenkinsBuild `json:"builds"`
	}{}
	if err := json.Unmarshal(data, &page); err != nil {
		return nil, fmt.Errorf("cannot unmarshal builds for job %q: %v", job, err)
	}
	jenkinsBuilds := make(map[string]JenkinsBuild)
	for _, jb := range page.Builds {
		buildID := jb.BuildID()
		// Ignore builds with missing buildId parameters.
		if buildID == "" {
			continue
		}
		jenkinsBuilds[buildID] = jb
	}
	return jenkinsBuilds, nil
}

// Abort aborts the provided Jenkins build for job.
func (c *Client) Abort(job string, build *JenkinsBuild) error {
	c.logger.Debugf("Abort(%v %v)", job, build.Number)

	resp, err := c.request(http.MethodPost, fmt.Sprintf("/job/%s/%d/stop", job, build.Number), nil, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("response not 2XX: %s", resp.Status)
	}
	return nil
}
