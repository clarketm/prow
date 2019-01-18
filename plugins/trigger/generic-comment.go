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

package trigger

import (
	"fmt"
	"regexp"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/labels"
	"k8s.io/test-infra/prow/plugins"
)

var okToTestRe = regexp.MustCompile(`(?m)^/ok-to-test\s*$`)
var testAllRe = regexp.MustCompile(`(?m)^/test all,?($|\s.*)`)
var retestRe = regexp.MustCompile(`(?m)^/retest\s*$`)

func handleGenericComment(c Client, trigger *plugins.Trigger, gc github.GenericCommentEvent) error {
	org := gc.Repo.Owner.Login
	repo := gc.Repo.Name
	number := gc.Number
	commentAuthor := gc.User.Login
	// Only take action when a comment is first created,
	// when it belongs to a PR,
	// and the PR is open.
	if gc.Action != github.GenericCommentActionCreated || !gc.IsPR || gc.IssueState != "open" {
		return nil
	}
	// Skip bot comments.
	botName, err := c.GitHubClient.BotName()
	if err != nil {
		return err
	}
	if commentAuthor == botName {
		return nil
	}

	pr, err := c.GitHubClient.GetPullRequest(org, repo, number)
	if err != nil {
		return err
	}

	// Skip untrusted users comments.
	trusted, err := TrustedUser(c.GitHubClient, trigger, commentAuthor, org, repo)
	if err != nil {
		return fmt.Errorf("error checking trust of %s: %v", commentAuthor, err)
	}
	var l []github.Label
	if !trusted {
		// Skip untrusted PRs.
		l, trusted, err = TrustedPullRequest(c.GitHubClient, trigger, gc.IssueAuthor.Login, org, repo, number, nil)
		if err != nil {
			return err
		}
		if !trusted {
			resp := fmt.Sprintf("Cannot trigger testing until a trusted user reviews the PR and leaves an `/ok-to-test` message.")
			c.Logger.Infof("Commenting \"%s\".", resp)
			return c.GitHubClient.CreateComment(org, repo, number, plugins.FormatResponseRaw(gc.Body, gc.HTMLURL, gc.User.Login, resp))
		}
	}

	// At this point we can trust the PR, so we eventually update labels.
	// Ensure we have labels before test, because TrustedPullRequest() won't be called
	// when commentAuthor is trusted.
	if l == nil {
		l, err = c.GitHubClient.GetIssueLabels(org, repo, number)
		if err != nil {
			return err
		}
	}
	needsOkToTest := HonorOkToTest(trigger) && okToTestRe.MatchString(gc.Body)
	if needsOkToTest && !github.HasLabel(labels.OkToTest, l) {
		if err := c.GitHubClient.AddLabel(org, repo, number, labels.OkToTest); err != nil {
			return err
		}
	}
	if (needsOkToTest || github.HasLabel(labels.OkToTest, l)) && github.HasLabel(labels.NeedsOkToTest, l) {
		if err := c.GitHubClient.RemoveLabel(org, repo, number, labels.NeedsOkToTest); err != nil {
			return err
		}
	}

	toTest, err := FilterPresubmits(HonorOkToTest(trigger), c.GitHubClient, gc.Body, pr, c.Config.Presubmits[gc.Repo.FullName])
	if err != nil {
		return err
	}
	fmt.Printf("got filtered presubmits: %v\n", toTest)
	return RunRequested(c, pr, toTest, gc.GUID)
}

func HonorOkToTest(trigger *plugins.Trigger) bool {
	return trigger == nil || !trigger.IgnoreOkToTest
}

type GitHubClient interface {
	GetCombinedStatus(org, repo, ref string) (*github.CombinedStatus, error)
	GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error)
}

// FilterPresubmits determines which presubmits should run. We only want to
// trigger jobs that should run, but the pool of jobs we filter to those that
// should run depends on the type of trigger we just got:
//  - if we got a /test all or an /ok-to-test, we want to consider any job
//    that doesn't explicitly require a human trigger comment; jobs will
//    default to not run unless we can determine that they should
//  - if we got a /retest, we only want to consider those jobs that have
//    already run and posted failing contexts to the PR or those jobs that
//    have not yet run but would otherwise match /test all; jobs will default
//    to run unless we can determine they shouldn't
//  - if we get a /test foo, we only want to consider those jobs that match;
//    jobs will default to run unless we can determine they shouldn't
func FilterPresubmits(honorOkToTest bool, gitHubClient GitHubClient, body string, pr *github.PullRequest, presubmits []config.Presubmit) ([]config.Presubmit, error) {
	org, repo, number, sha, branch := pr.Base.Repo.Owner.Login, pr.Base.Repo.Name, pr.Number, pr.Head.SHA, pr.Base.Ref
	filter, err := presubmitFilter(honorOkToTest, gitHubClient, body, org, repo, sha)
	if err != nil {
		return nil, err
	}

	var filteredPresubmits []config.Presubmit
	for _, presubmit := range presubmits {
		matches, defaults := filter(presubmit)
		if matches {
			shouldRun, err := presubmit.ShouldRun(branch, config.NewGitHubDeferredChangedFilesProvider(gitHubClient, org, repo, number), defaults)
			if err != nil {
				return nil, err
			}
			if shouldRun {
				filteredPresubmits = append(filteredPresubmits, presubmit)
			}
		}
	}
	return filteredPresubmits, nil
}

type statusGetter interface {
	GetCombinedStatus(org, repo, ref string) (*github.CombinedStatus, error)
}

func presubmitFilter(honorOkToTest bool, statusGetter statusGetter, body, org, repo, sha string) (func(config.Presubmit) (bool, bool), error) {
	// the filter determines if we should check whether a job should run and
	// what the default behavior should be for that check
	var filter func(config.Presubmit) (bool, bool)
	if (honorOkToTest && okToTestRe.MatchString(body)) || testAllRe.MatchString(body) {
		filter = func(p config.Presubmit) (bool, bool) {
			return !p.NeedsExplicitTrigger(), false
		}
	} else if retestRe.MatchString(body) {
		combinedStatus, err := statusGetter.GetCombinedStatus(org, repo, sha)
		if err != nil {
			return nil, err
		}
		allContexts := sets.NewString()
		failedContexts := sets.NewString()
		for _, status := range combinedStatus.Statuses {
			allContexts.Insert(status.Context)
			if status.State == github.StatusError || status.State == github.StatusFailure {
				failedContexts.Insert(status.Context)
			}
		}
		filter = func(p config.Presubmit) (bool, bool) {
			return failedContexts.Has(p.Context) || (!p.NeedsExplicitTrigger() && !allContexts.Has(p.Context)), true
		}
	} else {
		filter = func(p config.Presubmit) (bool, bool) {
			return p.TriggerMatches(body), true
		}
	}
	return filter, nil
}
