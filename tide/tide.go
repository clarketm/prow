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

// Package tide contains a controller for managing a tide pool of PRs. The
// controller will automatically retest PRs in the pool and merge them if they
// pass tests.
package tide

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shurcooL/githubql"
	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/git"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/pjutil"
	"k8s.io/test-infra/prow/tide/blockers"
)

type kubeClient interface {
	ListProwJobs(string) ([]kube.ProwJob, error)
	CreateProwJob(kube.ProwJob) (kube.ProwJob, error)
}

type githubClient interface {
	CreateStatus(string, string, string, github.Status) error
	GetCombinedStatus(org, repo, ref string) (*github.CombinedStatus, error)
	GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error)
	GetRef(string, string, string) (string, error)
	Merge(string, string, int, github.MergeDetails) error
	Query(context.Context, interface{}, map[string]interface{}) error
}

type contextChecker interface {
	// IsOptional tells whether a context is optional.
	IsOptional(string) bool
	// MissingRequiredContexts tells if required contexts are missing from the list of contexts provided.
	MissingRequiredContexts([]string) []string
}

// Controller knows how to sync PRs and PJs.
type Controller struct {
	logger *logrus.Entry
	ca     *config.Agent
	ghc    githubClient
	kc     kubeClient
	gc     *git.Client

	sc *statusController

	m     sync.Mutex
	pools []Pool

	// Cache from last sync loop. "org/repo#num:sha" -> files changed
	fileChangesCache map[string][]string
}

// Action represents what actions the controller can take. It will take
// exactly one action each sync.
type Action string

// Constants for various actions the controller might take
const (
	Wait         Action = "WAIT"
	Trigger             = "TRIGGER"
	TriggerBatch        = "TRIGGER_BATCH"
	Merge               = "MERGE"
	MergeBatch          = "MERGE_BATCH"
	PoolBlocked         = "POOL_BLOCKED"
)

// Pool represents information about a tide pool. There is one for every
// org/repo/branch combination that has PRs in the pool.
type Pool struct {
	Org    string
	Repo   string
	Branch string

	// PRs with passing tests, pending tests, and missing or failed tests.
	// Note that these results are rolled up. If all tests for a PR are passing
	// except for one pending, it will be in PendingPRs.
	SuccessPRs []PullRequest
	PendingPRs []PullRequest
	MissingPRs []PullRequest

	// Empty if there is no pending batch.
	BatchPending []PullRequest

	// Which action did we last take, and to what target(s), if any.
	Action   Action
	Target   []PullRequest
	Blockers []blockers.Blocker
}

// NewController makes a Controller out of the given clients.
func NewController(ghcSync, ghcStatus *github.Client, kc *kube.Client, ca *config.Agent, gc *git.Client, logger *logrus.Entry) *Controller {
	if logger == nil {
		logger = logrus.NewEntry(logrus.StandardLogger())
	}
	sc := &statusController{
		logger:         logger.WithField("controller", "status-update"),
		ghc:            ghcStatus,
		ca:             ca,
		newPoolPending: make(chan bool, 1),
		shutDown:       make(chan bool),
	}
	go sc.run()
	return &Controller{
		logger:           logger.WithField("controller", "sync"),
		ghc:              ghcSync,
		kc:               kc,
		ca:               ca,
		gc:               gc,
		sc:               sc,
		fileChangesCache: map[string][]string{},
	}
}

// Shutdown signals the statusController to stop working and waits for it to
// finish its last update loop before terminating.
// Controller.Sync() should not be used after this function is called.
func (c *Controller) Shutdown() {
	c.sc.shutdown()
}

func prKey(pr *PullRequest) string {
	return fmt.Sprintf("%s#%d", string(pr.Repository.NameWithOwner), int(pr.Number))
}

// org/repo#number -> pr
func byRepoAndNumber(prs []PullRequest) map[string]PullRequest {
	m := make(map[string]PullRequest)
	for _, pr := range prs {
		key := prKey(&pr)
		m[key] = pr
	}
	return m
}

// newExpectedContext creates a Context with Expected state.
func newExpectedContext(c string) Context {
	return Context{
		Context:     githubql.String(c),
		State:       githubql.StatusStateExpected,
		Description: githubql.String(""),
	}
}

// contextsToStrings converts a list Context to a list of string
func contextsToStrings(contexts []Context) []string {
	var names []string
	for _, c := range contexts {
		names = append(names, string(c.Context))
	}
	return names
}

// Sync runs one sync iteration.
func (c *Controller) Sync() error {
	ctx := context.Background()
	c.logger.Debug("Building tide pool.")
	pool := make(map[string]PullRequest)
	for _, q := range c.ca.Config().Tide.Queries {
		poolPRs, err := search(ctx, c.ghc, c.logger, q.Query())
		if err != nil {
			return err
		}
		for _, pr := range poolPRs {
			// Only keep PRs that are mergeable or haven't had mergeability computed.
			if pr.Mergeable != githubql.MergeableStateConflicting {
				pool[prKey(&pr)] = pr
			}
		}
	}
	// Notify statusController about the new pool.
	c.sc.Lock()
	c.sc.poolPRs = pool
	select {
	case c.sc.newPoolPending <- true:
	default:
	}
	c.sc.Unlock()

	var pjs []kube.ProwJob
	var blocks blockers.Blockers
	var err error
	if len(pool) > 0 {
		pjs, err = c.kc.ListProwJobs(kube.EmptySelector)
		if err != nil {
			return err
		}

		if label := c.ca.Config().Tide.BlockerLabel; label != "" {
			c.logger.Debugf("Searching for blocking issues (label %q).", label)
			orgs, repos := c.ca.Config().Tide.Queries.OrgsAndRepos()
			blocks, err = blockers.FindAll(c.ghc, c.logger, label, orgs, repos)
			if err != nil {
				return err
			}
		}
	}
	sps, err := c.dividePool(pool, pjs)
	if err != nil {
		return err
	}

	goroutines := c.ca.Config().Tide.MaxGoroutines
	if goroutines > len(sps) {
		goroutines = len(sps)
	}
	wg := &sync.WaitGroup{}
	wg.Add(goroutines)
	c.logger.Debugf("Firing up %d goroutines", goroutines)
	poolChan := make(chan Pool, len(sps))
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for sp := range sps {
				spBlocks := blocks.GetApplicable(sp.org, sp.repo, sp.branch)
				if pool, err := c.syncSubpool(sp, spBlocks); err != nil {
					sp.log.WithError(err).Errorf("Error syncing subpool.")
				} else {
					poolChan <- pool
				}
			}
		}()
	}
	wg.Wait()
	close(poolChan)

	pools := make([]Pool, 0, len(sps))
	for pool := range poolChan {
		pools = append(pools, pool)
	}
	c.m.Lock()
	defer c.m.Unlock()
	c.pools = pools
	return nil
}

func (c *Controller) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.m.Lock()
	defer c.m.Unlock()
	b, err := json.Marshal(c.pools)
	if err != nil {
		c.logger.WithError(err).Error("Encoding JSON.")
		b = []byte("[]")
	}
	if _, err = w.Write(b); err != nil {
		c.logger.WithError(err).Error("Writing JSON response.")
	}
}

type simpleState string

const (
	noneState    simpleState = "none"
	pendingState simpleState = "pending"
	successState simpleState = "success"
)

func toSimpleState(s kube.ProwJobState) simpleState {
	if s == kube.TriggeredState || s == kube.PendingState {
		return pendingState
	} else if s == kube.SuccessState {
		return successState
	}
	return noneState
}

// isPassingTests returns whether or not all contexts set on the PR except for
// the tide pool context are passing.
func isPassingTests(log *logrus.Entry, ghc githubClient, pr PullRequest, cc contextChecker) bool {
	log = log.WithFields(pr.logFields())
	contexts, err := headContexts(log, ghc, &pr)
	if err != nil {
		log.WithError(err).Error("Getting head commit status contexts.")
		// If we can't get the status of the commit, assume that it is failing.
		return false
	}
	return len(unsuccessfulContexts(contexts, cc)) == 0
}

// unsuccessfulContexts determines which contexts from the list that we care about are
// failed. For instance, we do not care about our own context.
// If the branchProtection is set to only check for required checks, we will skip
// all non-required tests. If required tests are missing from the list, they will be
// added to the list of failed contexts.
func unsuccessfulContexts(contexts []Context, cc contextChecker) []Context {
	var failed []Context
	for _, ctx := range contexts {
		if string(ctx.Context) == statusContext {
			continue
		}
		if cc.IsOptional(string(ctx.Context)) {
			continue
		}
		if ctx.State != githubql.StatusStateSuccess {
			failed = append(failed, ctx)
		}
	}
	for _, c := range cc.MissingRequiredContexts(contextsToStrings(contexts)) {
		failed = append(failed, newExpectedContext(c))
	}

	return failed
}

func pickSmallestPassingNumber(log *logrus.Entry, ghc githubClient, prs []PullRequest, cc contextChecker) (bool, PullRequest) {
	smallestNumber := -1
	var smallestPR PullRequest
	for _, pr := range prs {
		if smallestNumber != -1 && int(pr.Number) >= smallestNumber {
			continue
		}
		if len(pr.Commits.Nodes) < 1 {
			continue
		}
		if !isPassingTests(log, ghc, pr, cc) {
			continue
		}
		smallestNumber = int(pr.Number)
		smallestPR = pr
	}
	return smallestNumber > -1, smallestPR
}

// accumulateBatch returns a list of PRs that can be merged after passing batch
// testing, if any exist. It also returns a list of PRs currently being batch
// tested.
func accumulateBatch(presubmits map[int]sets.String, prs []PullRequest, pjs []kube.ProwJob) ([]PullRequest, []PullRequest) {
	if len(presubmits) == 0 {
		// Avoid accumulating batches when no presubmits are configured.
		return nil, nil
	}
	prNums := make(map[int]PullRequest)
	for _, pr := range prs {
		prNums[int(pr.Number)] = pr
	}
	type accState struct {
		prs       []PullRequest
		jobStates map[string]simpleState
		// Are the pull requests in the ref still acceptable? That is, do they
		// still point to the heads of the PRs?
		validPulls bool
	}
	states := make(map[string]*accState)
	for _, pj := range pjs {
		if pj.Spec.Type != kube.BatchJob {
			continue
		}
		// If any batch job is pending, return now.
		if toSimpleState(pj.Status.State) == pendingState {
			var pending []PullRequest
			for _, pull := range pj.Spec.Refs.Pulls {
				pending = append(pending, prNums[pull.Number])
			}
			return nil, pending
		}
		// Otherwise, accumulate results.
		ref := pj.Spec.Refs.String()
		if _, ok := states[ref]; !ok {
			states[ref] = &accState{
				jobStates:  make(map[string]simpleState),
				validPulls: true,
			}
			for _, pull := range pj.Spec.Refs.Pulls {
				if pr, ok := prNums[pull.Number]; ok && string(pr.HeadRefOID) == pull.SHA {
					states[ref].prs = append(states[ref].prs, pr)
				} else {
					states[ref].validPulls = false
					break
				}
			}
		}
		if !states[ref].validPulls {
			// The batch contains a PR ref that has changed. Skip it.
			continue
		}
		job := pj.Spec.Job
		if s, ok := states[ref].jobStates[job]; !ok || s == noneState {
			states[ref].jobStates[job] = toSimpleState(pj.Status.State)
		}
	}
	for _, state := range states {
		if !state.validPulls {
			continue
		}
		requiredPresubmits := sets.NewString()
		for _, pr := range state.prs {
			requiredPresubmits = requiredPresubmits.Union(presubmits[int(pr.Number)])
		}
		passesAll := true
		for _, p := range requiredPresubmits.List() {
			if s, ok := state.jobStates[p]; !ok || s != successState {
				passesAll = false
				continue
			}
		}
		if !passesAll {
			continue
		}
		return state.prs, nil
	}
	return nil, nil
}

// accumulate returns the supplied PRs sorted into three buckets based on their
// accumulated state across the presubmits.
func accumulate(presubmits map[int]sets.String, prs []PullRequest, pjs []kube.ProwJob) (successes, pendings, nones []PullRequest) {
	for _, pr := range prs {
		// Accumulate the best result for each job.
		psStates := make(map[string]simpleState)
		for _, pj := range pjs {
			if pj.Spec.Type != kube.PresubmitJob {
				continue
			}
			if pj.Spec.Refs.Pulls[0].Number != int(pr.Number) {
				continue
			}
			if pj.Spec.Refs.Pulls[0].SHA != string(pr.HeadRefOID) {
				continue
			}

			name := pj.Spec.Job
			oldState := psStates[name]
			newState := toSimpleState(pj.Status.State)
			if oldState == noneState || oldState == "" {
				psStates[name] = newState
			} else if oldState == pendingState && newState == successState {
				psStates[name] = successState
			}
		}
		// The overall result is the worst of the best.
		overallState := successState
		for _, ps := range presubmits[int(pr.Number)].List() {
			if s, ok := psStates[ps]; s == noneState || !ok {
				overallState = noneState
				break
			} else if s == pendingState {
				overallState = pendingState
			}
		}
		if overallState == successState {
			successes = append(successes, pr)
		} else if overallState == pendingState {
			pendings = append(pendings, pr)
		} else {
			nones = append(nones, pr)
		}
	}
	return
}

func prNumbers(prs []PullRequest) []int {
	var nums []int
	for _, pr := range prs {
		nums = append(nums, int(pr.Number))
	}
	return nums
}

func (c *Controller) pickBatch(sp subpool, cc contextChecker) ([]PullRequest, error) {
	r, err := c.gc.Clone(sp.org + "/" + sp.repo)
	if err != nil {
		return nil, err
	}
	defer r.Clean()
	if err := r.Config("user.name", "prow"); err != nil {
		return nil, err
	}
	if err := r.Config("user.email", "prow@localhost"); err != nil {
		return nil, err
	}
	if err := r.Config("commit.gpgsign", "false"); err != nil {
		sp.log.Warningf("Cannot set gpgsign=false in gitconfig: %v", err)
	}
	if err := r.Checkout(sp.sha); err != nil {
		return nil, err
	}

	// we must choose the oldest PRs for the batch
	sort.Slice(sp.prs, func(i, j int) bool { return sp.prs[i].Number < sp.prs[j].Number })

	var res []PullRequest
	for _, pr := range sp.prs {
		if !isPassingTests(sp.log, c.ghc, pr, cc) {
			continue
		}
		if ok, err := r.Merge(string(pr.HeadRefOID)); err != nil {
			// we failed to abort the merge and our git client is
			// in a bad state; it must be cleaned before we try again
			return nil, err
		} else if ok {
			res = append(res, pr)
			// TODO: Make this configurable per subpool.
			if len(res) == 5 {
				break
			}
		}
	}
	return res, nil
}

func (c *Controller) mergePRs(sp subpool, prs []PullRequest) error {
	maxRetries := 3
	for i, pr := range prs {
		backoff := time.Second * 4
		log := sp.log.WithFields(pr.logFields())
		for retry := 0; retry < maxRetries; retry++ {
			if err := c.ghc.Merge(sp.org, sp.repo, int(pr.Number), github.MergeDetails{
				SHA:         string(pr.HeadRefOID),
				MergeMethod: string(c.ca.Config().Tide.MergeMethod(sp.org, sp.repo)),
			}); err != nil {
				if _, ok := err.(github.ModifiedHeadError); ok {
					// This is a possible source of incorrect behavior. If someone
					// modifies their PR as we try to merge it in a batch then we
					// end up in an untested state. This is unlikely to cause any
					// real problems.
					log.WithError(err).Warning("Merge failed: PR was modified.")
					break
				} else if _, ok = err.(github.UnmergablePRBaseChangedError); ok {
					// Github complained that the base branch was modified. This is a
					// strange error because the API doesn't even allow the request to
					// specify the base branch sha, only the head sha.
					// We suspect that github is complaining because we are making the
					// merge requests too rapidly and it cannot recompute mergability
					// in time. https://github.com/kubernetes/test-infra/issues/5171
					// We handle this by sleeping for a few seconds before trying to
					// merge again.
					log.WithError(err).Warning("Merge failed: Base branch was modified.")
					if retry+1 < maxRetries {
						time.Sleep(backoff)
						backoff *= 2
					}
				} else if _, ok = err.(github.UnauthorizedToPushError); ok {
					// Github let us know that the token used cannot push to the branch.
					// Even if the robot is set up to have write access to the repo, an
					// overzealous branch protection setting will not allow the robot to
					// push to a specific branch.
					log.WithError(err).Error("Merge failed: Branch needs to be configured to allow this robot to push.")
					break
				} else if _, ok = err.(github.UnmergablePRError); ok {
					log.WithError(err).Error("Merge failed: PR is unmergable. How did it pass tests?!")
					break
				} else {
					log.WithError(err).Error("Merge failed.")
					return err
				}
			} else {
				log.Info("Merged.")
				// If we have more PRs to merge, sleep to give Github time to recalculate
				// mergeability.
				if i+1 < len(prs) {
					time.Sleep(time.Second * 3)
				}
				break
			}
		}
	}
	return nil
}

func (c *Controller) trigger(sp subpool, presubmits map[int]sets.String, prs []PullRequest) error {
	requiredJobs := sets.NewString()
	for _, pr := range prs {
		requiredJobs = requiredJobs.Union(presubmits[int(pr.Number)])
	}

	// TODO(cjwagner): DRY this out when generalizing triggering code (and code to determine required and to-run jobs).
	for _, ps := range c.ca.Config().Presubmits[sp.org+"/"+sp.repo] {
		if ps.SkipReport || !ps.RunsAgainstBranch(sp.branch) || !requiredJobs.Has(ps.Name) {
			continue
		}

		refs := kube.Refs{
			Org:     sp.org,
			Repo:    sp.repo,
			BaseRef: sp.branch,
			BaseSHA: sp.sha,
		}
		for _, pr := range prs {
			refs.Pulls = append(
				refs.Pulls,
				kube.Pull{
					Number: int(pr.Number),
					Author: string(pr.Author.Login),
					SHA:    string(pr.HeadRefOID),
				},
			)
		}
		var spec kube.ProwJobSpec
		if len(prs) == 1 {
			spec = pjutil.PresubmitSpec(ps, refs)
		} else {
			spec = pjutil.BatchSpec(ps, refs)
		}
		pj := pjutil.NewProwJob(spec, ps.Labels)
		if _, err := c.kc.CreateProwJob(pj); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) takeAction(sp subpool, presubmits map[int]sets.String, batchPending, successes, pendings, nones, batchMerges []PullRequest, cc contextChecker) (Action, []PullRequest, error) {
	// Merge the batch!
	if len(batchMerges) > 0 {
		return MergeBatch, batchMerges, c.mergePRs(sp, batchMerges)
	}
	// Do not merge PRs while waiting for a batch to complete. We don't want to
	// invalidate the old batch result.
	if len(successes) > 0 && len(batchPending) == 0 {
		if ok, pr := pickSmallestPassingNumber(sp.log, c.ghc, successes, cc); ok {
			return Merge, []PullRequest{pr}, c.mergePRs(sp, []PullRequest{pr})
		}
	}
	// If no presubmits are configured, just wait.
	if len(presubmits) == 0 {
		return Wait, nil, nil
	}
	// If we have no serial jobs pending or successful, trigger one.
	if len(nones) > 0 && len(pendings) == 0 && len(successes) == 0 {
		if ok, pr := pickSmallestPassingNumber(sp.log, c.ghc, nones, cc); ok {
			return Trigger, []PullRequest{pr}, c.trigger(sp, presubmits, []PullRequest{pr})
		}
	}
	// If we have no batch, trigger one.
	if len(sp.prs) > 1 && len(batchPending) == 0 {
		batch, err := c.pickBatch(sp, cc)
		if err != nil {
			return Wait, nil, err
		}
		if len(batch) > 1 {
			return TriggerBatch, batch, c.trigger(sp, presubmits, batch)
		}
	}
	return Wait, nil, nil
}

func (c *Controller) presubmitsByPull(sp subpool) (map[int]sets.String, error) {
	presubmits := make(map[int]sets.String, len(sp.prs))
	record := func(num int, job string) {
		if jobs, ok := presubmits[num]; ok {
			jobs.Insert(job)
		} else {
			presubmits[num] = sets.NewString(job)
		}
	}
	// nextChangeCache caches file change info that is relevant this sync for use next sync.
	nextChangeCache := map[string][]string{}
	defer func() {
		c.fileChangesCache = nextChangeCache
	}()

	for _, ps := range c.ca.Config().Presubmits[sp.org+"/"+sp.repo] {
		if !ps.ContextRequired() || !ps.RunsAgainstBranch(sp.branch) {
			continue
		}

		if ps.AlwaysRun {
			// Every PR requires this job.
			for _, pr := range sp.prs {
				record(int(pr.Number), ps.Name)
			}
		} else if ps.RunIfChanged != "" {
			// This is a run if changed job so we need to check if each PR requires it.
			for _, pr := range sp.prs {
				cacheKey := fmt.Sprintf("%s/%s#%d:%s", sp.org, sp.repo, int(pr.Number), string(pr.HeadRefOID))
				changedFiles, ok := c.fileChangesCache[cacheKey]
				if !ok {
					changes, err := c.ghc.GetPullRequestChanges(sp.org, sp.repo, int(pr.Number))
					if err != nil {
						return nil, fmt.Errorf("error getting PR changes for #%d: %v", int(pr.Number), err)
					}
					changedFiles = make([]string, 0, len(changes))
					for _, change := range changes {
						changedFiles = append(changedFiles, change.Filename)
					}
					c.fileChangesCache[cacheKey] = changedFiles
				}
				nextChangeCache[cacheKey] = changedFiles
				if ps.RunsAgainstChanges(changedFiles) {
					record(int(pr.Number), ps.Name)
				}
			}
		}
	}
	return presubmits, nil
}

func (c *Controller) syncSubpool(sp subpool, blocks []blockers.Blocker) (Pool, error) {
	sp.log.Infof("Syncing subpool: %d PRs, %d PJs.", len(sp.prs), len(sp.pjs))
	presubmits, err := c.presubmitsByPull(sp)
	if err != nil {
		return Pool{}, fmt.Errorf("error determining required presubmits: %v", err)
	}
	cr, err := c.ca.Config().GetTideContextPolicy(sp.org, sp.repo, sp.branch)
	if err != nil {
		return Pool{}, fmt.Errorf("error parsing tide context options: %v", err)
	}
	successes, pendings, nones := accumulate(presubmits, sp.prs, sp.pjs)
	batchMerge, batchPending := accumulateBatch(presubmits, sp.prs, sp.pjs)
	sp.log.WithFields(logrus.Fields{
		"prs-passing":   prNumbers(successes),
		"prs-pending":   prNumbers(pendings),
		"prs-missing":   prNumbers(nones),
		"batch-passing": prNumbers(batchMerge),
		"batch-pending": prNumbers(batchPending),
	}).Info("Subpool accumulated.")

	var act Action
	var targets []PullRequest
	if len(blocks) > 0 {
		act = PoolBlocked
	} else {
		act, targets, err = c.takeAction(sp, presubmits, batchPending, successes, pendings, nones, batchMerge, &cr)
	}

	sp.log.WithFields(logrus.Fields{
		"action":  string(act),
		"targets": prNumbers(targets),
	}).Info("Subpool synced.")
	return Pool{
			Org:    sp.org,
			Repo:   sp.repo,
			Branch: sp.branch,

			SuccessPRs: successes,
			PendingPRs: pendings,
			MissingPRs: nones,

			BatchPending: batchPending,

			Action:   act,
			Target:   targets,
			Blockers: blocks,
		},
		err
}

type subpool struct {
	log    *logrus.Entry
	org    string
	repo   string
	branch string
	sha    string
	pjs    []kube.ProwJob
	prs    []PullRequest
}

// dividePool splits up the list of pull requests and prow jobs into a group
// per repo and branch. It only keeps ProwJobs that match the latest branch.
func (c *Controller) dividePool(pool map[string]PullRequest, pjs []kube.ProwJob) (chan subpool, error) {
	sps := make(map[string]*subpool)
	for _, pr := range pool {
		org := string(pr.Repository.Owner.Login)
		repo := string(pr.Repository.Name)
		branch := string(pr.BaseRef.Name)
		branchRef := string(pr.BaseRef.Prefix) + string(pr.BaseRef.Name)
		fn := fmt.Sprintf("%s/%s %s", org, repo, branch)
		if sps[fn] == nil {
			sha, err := c.ghc.GetRef(org, repo, strings.TrimPrefix(branchRef, "refs/"))
			if err != nil {
				return nil, err
			}
			sps[fn] = &subpool{
				log: c.logger.WithFields(logrus.Fields{
					"org":      org,
					"repo":     repo,
					"branch":   branch,
					"base-sha": sha,
				}),
				org:    org,
				repo:   repo,
				branch: branch,
				sha:    sha,
			}
		}
		sps[fn].prs = append(sps[fn].prs, pr)
	}
	for _, pj := range pjs {
		if pj.Spec.Type != kube.PresubmitJob && pj.Spec.Type != kube.BatchJob {
			continue
		}
		fn := fmt.Sprintf("%s/%s %s", pj.Spec.Refs.Org, pj.Spec.Refs.Repo, pj.Spec.Refs.BaseRef)
		if sps[fn] == nil || pj.Spec.Refs.BaseSHA != sps[fn].sha {
			continue
		}
		sps[fn].pjs = append(sps[fn].pjs, pj)
	}
	ret := make(chan subpool, len(sps))
	for _, sp := range sps {
		ret <- *sp
	}
	close(ret)
	return ret, nil
}

func search(ctx context.Context, ghc githubClient, log *logrus.Entry, q string) ([]PullRequest, error) {
	var ret []PullRequest
	vars := map[string]interface{}{
		"query":        githubql.String(q),
		"searchCursor": (*githubql.String)(nil),
	}
	var totalCost int
	var remaining int
	for {
		sq := searchQuery{}
		if err := ghc.Query(ctx, &sq, vars); err != nil {
			return nil, err
		}
		totalCost += int(sq.RateLimit.Cost)
		remaining = int(sq.RateLimit.Remaining)
		for _, n := range sq.Search.Nodes {
			ret = append(ret, n.PullRequest)
		}
		if !sq.Search.PageInfo.HasNextPage {
			break
		}
		vars["searchCursor"] = githubql.NewString(sq.Search.PageInfo.EndCursor)
	}
	log.Debugf("Search for query \"%s\" cost %d point(s). %d remaining.", q, totalCost, remaining)
	return ret, nil
}

// PullRequest holds graphql data about a PR, including its commits and their contexts.
type PullRequest struct {
	Number githubql.Int
	Author struct {
		Login githubql.String
	}
	BaseRef struct {
		Name   githubql.String
		Prefix githubql.String
	}
	HeadRefName githubql.String `graphql:"headRefName"`
	HeadRefOID  githubql.String `graphql:"headRefOid"`
	HeadRef     *struct {
		Target struct {
			Commit Commit `graphql:"... on Commit"`
		}
	}
	Mergeable  githubql.MergeableState
	Repository struct {
		Name          githubql.String
		NameWithOwner githubql.String
		Owner         struct {
			Login githubql.String
		}
	}
	Commits struct {
		Nodes []struct {
			Commit Commit
		}
		// See the 'headContexts' function for details.
	} `graphql:"commits(last: 1)"`
	Labels struct {
		Nodes []struct {
			Name githubql.String
		}
	} `graphql:"labels(first: 100)"`
	Milestone *struct {
		Title githubql.String
	}
}

// Commit holds graphql data about commits and which contexts they have
type Commit struct {
	Status struct {
		Contexts []Context
	}
	OID githubql.String `graphql:"oid"`
}

// Context holds graphql response data for github contexts.
type Context struct {
	Context     githubql.String
	Description githubql.String
	State       githubql.StatusState
}

type searchQuery struct {
	RateLimit struct {
		Cost      githubql.Int
		Remaining githubql.Int
	}
	Search struct {
		PageInfo struct {
			HasNextPage githubql.Boolean
			EndCursor   githubql.String
		}
		Nodes []struct {
			PullRequest PullRequest `graphql:"... on PullRequest"`
		}
	} `graphql:"search(type: ISSUE, first: 100, after: $searchCursor, query: $query)"`
}

func (pr *PullRequest) logFields() logrus.Fields {
	return logrus.Fields{
		"org":  string(pr.Repository.Owner.Login),
		"repo": string(pr.Repository.Name),
		"pr":   int(pr.Number),
		"sha":  string(pr.HeadRefOID),
	}
}

// headContexts gets the status contexts for the commit with OID == pr.HeadRefOID
//
// There is no single way to use the GitHub Graphql API to get the status contexts
// for the head commit of PR. There are 2 ways that each only work under certain
// conditions and sometimes neither of those will work and we must fall back to
// the REST API. Here is our process:
//
// First, we try to get the statuses via the `headRef` field. This works most PRs,
// only PRs with a deleted head ref will need to continue on.
//
// If the head ref has been deleted, we can still get status contexts by looking
// at the list of commits for the PR. Unfortunately the 'last' commit ordering
// is determined by author date not commit date so if commits are reordered
// non-chronologically on the PR branch, the 'last' commit isn't necessarily the
// logically last commit. Most PRs won't have reorder commits so in most cases
// we can just use the statuses from this 'last' commit.
//
// If the worst case occurs and neither of the above cases is suitable we have
// to use the REST API to get the head commit statuses. This costs an extra API
// token every sync loop.
//
// Here are some issues on GitHub's support forum that describe why this gross
// work around is necessary.
// https://platform.github.community/t/some-prs-are-missing-head-refs/4586
// https://platform.github.community/t/github-commits-returned-in-the-incorrect-order-how-to-get-the-head-commit-for-statuses/4130
func headContexts(log *logrus.Entry, ghc githubClient, pr *PullRequest) ([]Context, error) {
	if contexts, ok := headContextsNoCost(pr); ok {
		return contexts, nil
	}

	// We didn't get the head commit from the query (the PR's commits must not be
	// logically ordered) so we need to specifically ask Github for the status
	// and coerce it to a graphql type.
	org := string(pr.Repository.Owner.Login)
	repo := string(pr.Repository.Name)
	log.Warnf("HeadRef was missing and 'last' commit is not the logical last commit. Querying Github...")
	combined, err := ghc.GetCombinedStatus(org, repo, string(pr.HeadRefOID))
	if err != nil {
		return nil, fmt.Errorf("failed to get the combined status: %v", err)
	}
	contexts := make([]Context, 0, len(combined.Statuses))
	for _, status := range combined.Statuses {
		contexts = append(
			contexts,
			Context{
				Context:     githubql.String(status.Context),
				Description: githubql.String(status.Description),
				State:       githubql.StatusState(strings.ToUpper(status.State)),
			},
		)
	}
	return contexts, nil
}

// headContextsNoCost tries to get the head commit contexts from the PullRequest
// struct without making additional API calls. It returns the contexts if found
// and a bool indicating success.
func headContextsNoCost(pr *PullRequest) ([]Context, bool) {
	if pr.HeadRef != nil {
		return pr.HeadRef.Target.Commit.Status.Contexts, true
	}
	for _, node := range pr.Commits.Nodes {
		if node.Commit.OID == pr.HeadRefOID {
			return node.Commit.Status.Contexts, true
		}
	}
	return nil, false
}
