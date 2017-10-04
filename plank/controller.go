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

package plank

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"
	"time"

	"github.com/bwmarrin/snowflake"
	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/pjutil"
	reportlib "k8s.io/test-infra/prow/report"
)

const (
	testInfra = "https://github.com/kubernetes/test-infra/issues"

	// maxSyncRoutines is the maximum number of goroutines
	// that will be active at any one time for the sync
	maxSyncRoutines = 20
)

type kubeClient interface {
	CreateProwJob(kube.ProwJob) (kube.ProwJob, error)
	ListProwJobs(map[string]string) ([]kube.ProwJob, error)
	ReplaceProwJob(string, kube.ProwJob) (kube.ProwJob, error)

	CreatePod(kube.Pod) (kube.Pod, error)
	ListPods(map[string]string) ([]kube.Pod, error)
	DeletePod(string) error
}

type githubClient interface {
	BotName() (string, error)
	CreateStatus(org, repo, ref string, s github.Status) error
	ListIssueComments(org, repo string, number int) ([]github.IssueComment, error)
	CreateComment(org, repo string, number int, comment string) error
	DeleteComment(org, repo string, ID int) error
	EditComment(org, repo string, ID int, comment string) error
	GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error)
}

type configAgent interface {
	Config() *config.Config
}

// TODO: Dry this out
type syncFn func(pj kube.ProwJob, pm map[string]kube.Pod, reports chan<- kube.ProwJob) error

// Controller manages ProwJobs.
type Controller struct {
	kc     kubeClient
	pkc    kubeClient
	ghc    githubClient
	ca     configAgent
	node   *snowflake.Node
	totURL string

	lock sync.RWMutex
	// pendingJobs is a short-lived cache that helps in limiting
	// the maximum concurrency of jobs.
	pendingJobs map[string]int
}

// canExecuteConcurrently checks whether the provided ProwJob can
// be executed concurrently.
func (c *Controller) canExecuteConcurrently(pj *kube.ProwJob) bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	if max := c.ca.Config().Plank.MaxConcurrency; max > 0 {
		var running int
		for _, num := range c.pendingJobs {
			running += num
		}
		if running >= max {
			logrus.Infof("Not starting another job, already %d running.", running)
			return false
		}
	}

	if pj.Spec.MaxConcurrency == 0 {
		c.pendingJobs[pj.Spec.Job]++
		return true
	}

	numPending := c.pendingJobs[pj.Spec.Job]
	if numPending >= pj.Spec.MaxConcurrency {
		logrus.WithField("job", pj.Spec.Job).Infof("Not starting another instance of %s, already %d running.", pj.Spec.Job, numPending)
		return false
	}
	c.pendingJobs[pj.Spec.Job]++
	return true
}

// incrementNumPendingJobs increments the amount of
// pending ProwJobs for the given job identifier
func (c *Controller) incrementNumPendingJobs(job string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.pendingJobs[job]++
}

// NewController creates a new Controller from the provided clients.
func NewController(kc, pkc *kube.Client, ghc *github.Client, ca *config.Agent, totURL string) (*Controller, error) {
	n, err := snowflake.NewNode(1)
	if err != nil {
		return nil, err
	}
	return &Controller{
		kc:          kc,
		pkc:         pkc,
		ghc:         ghc,
		ca:          ca,
		node:        n,
		pendingJobs: make(map[string]int),
		lock:        sync.RWMutex{},
		totURL:      totURL,
	}, nil
}

// Sync does one sync iteration.
func (c *Controller) Sync() error {
	pjs, err := c.kc.ListProwJobs(nil)
	if err != nil {
		return fmt.Errorf("error listing prow jobs: %v", err)
	}
	labels := map[string]string{kube.CreatedByProw: "true"}
	pods, err := c.pkc.ListPods(labels)
	if err != nil {
		return fmt.Errorf("error listing pods: %v", err)
	}
	pm := map[string]kube.Pod{}
	for _, pod := range pods {
		pm[pod.Metadata.Name] = pod
	}

	var k8sJobs []kube.ProwJob
	for _, pj := range pjs {
		if pj.Spec.Agent == kube.KubernetesAgent {
			k8sJobs = append(k8sJobs, pj)
		}
	}
	pjs = k8sJobs

	var syncErrs []error
	if err := c.terminateDupes(pjs); err != nil {
		syncErrs = append(syncErrs, err)
	}

	pendingCh, nonPendingCh := pjutil.PartitionPending(pjs)
	errCh := make(chan error, len(pjs))
	reportCh := make(chan kube.ProwJob, len(pjs))

	// Reinstantiate on every resync of the controller instead of trying
	// to keep this in sync with the state of the world.
	c.pendingJobs = make(map[string]int)
	// Sync pending jobs first so we can determine what is the maximum
	// number of new jobs we can trigger when syncing the non-pendings.
	syncProwJobs(c.syncPendingJob, pendingCh, reportCh, errCh, pm)
	syncProwJobs(c.syncNonPendingJob, nonPendingCh, reportCh, errCh, pm)

	close(errCh)
	close(reportCh)

	for err := range errCh {
		syncErrs = append(syncErrs, err)
	}

	var reportErrs []error
	reportTemplate := c.ca.Config().Plank.ReportTemplate
	for report := range reportCh {
		if err := reportlib.Report(c.ghc, reportTemplate, report); err != nil {
			reportErrs = append(reportErrs, err)
		}
	}

	if len(syncErrs) == 0 && len(reportErrs) == 0 {
		return nil
	}
	return fmt.Errorf("errors syncing: %v, errors reporting: %v", syncErrs, reportErrs)
}

// terminateDupes aborts presubmits that have a newer version. It modifies pjs
// in-place when it aborts.
// TODO: Dry this out - need to ensure we can abstract children cancellation first.
func (c *Controller) terminateDupes(pjs []kube.ProwJob) error {
	// "job org/repo#number" -> newest job
	dupes := make(map[string]int)
	for i, pj := range pjs {
		if pj.Complete() || pj.Spec.Type != kube.PresubmitJob {
			continue
		}
		n := fmt.Sprintf("%s %s/%s#%d", pj.Spec.Job, pj.Spec.Refs.Org, pj.Spec.Refs.Repo, pj.Spec.Refs.Pulls[0].Number)
		prev, ok := dupes[n]
		if !ok {
			dupes[n] = i
			continue
		}
		cancelIndex := i
		if pjs[prev].Status.StartTime.Before(pj.Status.StartTime) {
			cancelIndex = prev
			dupes[n] = i
		}
		toCancel := pjs[cancelIndex]
		toCancel.Status.CompletionTime = time.Now()
		toCancel.Status.State = kube.AbortedState
		npj, err := c.kc.ReplaceProwJob(toCancel.Metadata.Name, toCancel)
		if err != nil {
			return err
		}
		pjs[cancelIndex] = npj
	}
	return nil
}

// TODO: Dry this out
func syncProwJobs(syncFn syncFn, jobs <-chan kube.ProwJob, reports chan<- kube.ProwJob, syncErrors chan<- error, pm map[string]kube.Pod) {
	wg := &sync.WaitGroup{}
	wg.Add(maxSyncRoutines)
	for i := 0; i < maxSyncRoutines; i++ {
		go func(jobs <-chan kube.ProwJob) {
			defer wg.Done()
			for pj := range jobs {
				if err := syncFn(pj, pm, reports); err != nil {
					syncErrors <- err
				}
			}
		}(jobs)
	}
	wg.Wait()
}

func (c *Controller) syncPendingJob(pj kube.ProwJob, pm map[string]kube.Pod, reports chan<- kube.ProwJob) error {
	pod, podExists := pm[pj.Metadata.Name]
	if !podExists {
		c.incrementNumPendingJobs(pj.Spec.Job)
		// Pod is missing. This can happen in case we deleted the previous pod because
		// it was stuck in Unknown/Evicted state due to a node problem or the pod was
		// deleted manually. Start a new pod.
		id, pn, err := c.startPod(pj)
		if err != nil {
			_, isUnprocessable := err.(kube.UnprocessableEntityError)
			if !isUnprocessable {
				return fmt.Errorf("error starting pod: %v", err)
			}
			pj.Status.State = kube.ErrorState
			pj.Status.CompletionTime = time.Now()
			pj.Status.Description = "Job cannot be processed."
			logrus.WithField("job", pj.Spec.Job).WithError(err).Warning("Unprocessable pod.")
		} else {
			pj.Status.BuildID = id
			pj.Status.PodName = pn
		}
	} else {
		switch pod.Status.Phase {
		case kube.PodUnknown:
			c.incrementNumPendingJobs(pj.Spec.Job)
			// Pod is in Unknown state. This can happen if there is a problem with
			// the node. Delete the old pod, we'll start a new one next loop.
			return c.pkc.DeletePod(pj.Metadata.Name)

		case kube.PodSucceeded:
			// Pod succeeded. Update ProwJob, talk to GitHub, and start next jobs.
			pj.Status.CompletionTime = time.Now()
			pj.Status.State = kube.SuccessState
			pj.Status.Description = "Job succeeded."
			for _, nj := range pj.Spec.RunAfterSuccess {
				child := pjutil.NewProwJob(nj)
				if !RunAfterSuccessCanRun(&pj, &child, c.ca, c.ghc) {
					continue
				}
				if _, err := c.kc.CreateProwJob(pjutil.NewProwJob(nj)); err != nil {
					return fmt.Errorf("error starting next prowjob: %v", err)
				}
			}

		case kube.PodFailed:
			if pod.Status.Reason == kube.Evicted {
				c.incrementNumPendingJobs(pj.Spec.Job)
				// Pod was evicted. We will recreate it in the next resync.
				return c.pkc.DeletePod(pj.Metadata.Name)
			}
			// Pod failed. Update ProwJob, talk to GitHub.
			pj.Status.CompletionTime = time.Now()
			pj.Status.State = kube.FailureState
			pj.Status.Description = "Job failed."

		default:
			// Pod is running. Do nothing.
			c.incrementNumPendingJobs(pj.Spec.Job)
			return nil
		}
	}

	var b bytes.Buffer
	if err := c.ca.Config().Plank.JobURLTemplate.Execute(&b, &pj); err != nil {
		return fmt.Errorf("error executing URL template: %v", err)
	}
	pj.Status.URL = b.String()
	reports <- pj

	_, err := c.kc.ReplaceProwJob(pj.Metadata.Name, pj)
	return err
}

func (c *Controller) syncNonPendingJob(pj kube.ProwJob, pm map[string]kube.Pod, reports chan<- kube.ProwJob) error {
	if pj.Complete() {
		return nil
	}

	// The rest are new prowjobs.

	var id, pn string
	pod, podExists := pm[pj.Metadata.Name]
	// We may end up in a state where the pod exists but the prowjob is not
	// updated to pending if we successfully create a new pod in a previous
	// sync but the prowjob update fails. Simply ignore creating a new pod
	// and rerun the prowjob update.
	if !podExists {
		// Do not start more jobs than specified.
		if !c.canExecuteConcurrently(&pj) {
			return nil
		}
		// We haven't started the pod yet. Do so.
		var err error
		id, pn, err = c.startPod(pj)
		if err != nil {
			_, isUnprocessable := err.(kube.UnprocessableEntityError)
			if !isUnprocessable {
				return fmt.Errorf("error starting pod: %v", err)
			}
			pj.Status.State = kube.ErrorState
			pj.Status.CompletionTime = time.Now()
			pj.Status.Description = "Job cannot be processed."
			logrus.WithField("job", pj.Spec.Job).WithError(err).Warning("Unprocessable pod.")
		}
	} else {
		id = getPodBuildID(&pod)
		pn = pod.Metadata.Name
	}

	if pj.Status.State == kube.TriggeredState {
		// BuildID needs to be set before we execute the job url template.
		pj.Status.BuildID = id
		pj.Status.State = kube.PendingState
		pj.Status.PodName = pn
		pj.Status.Description = "Job triggered."
		var b bytes.Buffer
		if err := c.ca.Config().Plank.JobURLTemplate.Execute(&b, &pj); err != nil {
			return fmt.Errorf("error executing URL template: %v", err)
		}
		pj.Status.URL = b.String()
	}
	reports <- pj

	_, err := c.kc.ReplaceProwJob(pj.Metadata.Name, pj)
	return err
}

// TODO: No need to return the pod name since we already have the
// prowjob in the call site.
func (c *Controller) startPod(pj kube.ProwJob) (string, string, error) {
	buildID, err := c.getBuildID(pj.Spec.Job)
	if err != nil {
		return "", "", fmt.Errorf("error getting build ID: %v", err)
	}

	pod := pjutil.ProwJobToPod(pj, buildID)

	actual, err := c.pkc.CreatePod(*pod)
	if err != nil {
		return "", "", err
	}
	return buildID, actual.Metadata.Name, nil
}

func (c *Controller) getBuildID(name string) (string, error) {
	if c.totURL == "" {
		return c.node.Generate().String(), nil
	}
	var err error
	url := c.totURL + "/vend/" + name
	for retries := 0; retries < 60; retries++ {
		if retries > 0 {
			time.Sleep(2 * time.Second)
		}
		var resp *http.Response
		resp, err = http.Get(url)
		if err != nil {
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			continue
		}
		if buf, err := ioutil.ReadAll(resp.Body); err == nil {
			return string(buf), nil
		}
		return "", err
	}
	return "", err
}

func getPodBuildID(pod *kube.Pod) string {
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "BUILD_NUMBER" {
			return env.Value
		}
	}
	logrus.Warningf("BUILD_NUMBER was not found in pod %q: streaming logs from deck will not work", pod.Metadata.Name)
	return ""
}

// RunAfterSuccessCanRun returns whether a child job (specified as run_after_success in the
// prow config) can run once its parent job succeeds. The only case we will not run a child job
// is when it is a presubmit job and has a run_if_changed regural expression specified which does
// not match the changed filenames in the pull request the job was meant to run for.
// TODO: Collapse with Jenkins, impossible to reuse as is due to the interfaces.
func RunAfterSuccessCanRun(parent, child *kube.ProwJob, c configAgent, ghc githubClient) bool {
	if parent.Spec.Type != kube.PresubmitJob {
		return true
	}

	// TODO: Make sure that parent and child have always the same org/repo.
	org := parent.Spec.Refs.Org
	repo := parent.Spec.Refs.Repo
	prNum := parent.Spec.Refs.Pulls[0].Number

	ps := c.Config().GetPresubmit(org+"/"+repo, child.Spec.Job)
	if ps == nil {
		// The config has changed ever since we started the parent.
		// Not sure what is more correct here. Run the child for now.
		return true
	}
	if ps.RunIfChanged == "" {
		return true
	}
	changesFull, err := ghc.GetPullRequestChanges(org, repo, prNum)
	if err != nil {
		logrus.Warningf("Cannot get PR changes for %d: %v", prNum, err)
		return true
	}
	// We only care about the filenames here
	var changes []string
	for _, change := range changesFull {
		changes = append(changes, change.Filename)
	}
	return ps.RunsAgainstChanges(changes)
}
