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

// Package docsnoretest contains a Prow plugin which manages a label indicating
// whether a given pull requests only changes documentation.  In such cases it
// would not need to be retested.
package docsnoretest

import (
	"fmt"
	"path"
	"regexp"

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/plugins"
)

const (
	pluginName      = "docs-no-retest"
	labelSkipRetest = "retest-not-required-docs-only"
)

var (
	docFilesRegex     = regexp.MustCompile(`^.*\.(md|png|svg|dia)$`)
	ownersFilesRegex  = regexp.MustCompile(`^OWNERS$`)
	licenseFilesRegex = regexp.MustCompile(`^LICENSE$`)
)

func init() {
	plugins.RegisterPullRequestHandler(pluginName, handlePullRequest, nil)
}

func handlePullRequest(pc plugins.PluginClient, pe github.PullRequestEvent) error {
	return handlePR(pc.GitHubClient, pe)
}

// Strict subset of *github.Client methods.
type githubClient interface {
	AddLabel(owner, repo string, number int, label string) error
	RemoveLabel(owner, repo string, number int, label string) error
	GetIssueLabels(org, repo string, number int) ([]github.Label, error)
	GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error)
}

func handlePR(gc githubClient, pe github.PullRequestEvent) error {
	var (
		owner = pe.PullRequest.Base.Repo.Owner.Login
		repo  = pe.PullRequest.Base.Repo.Name
		num   = pe.PullRequest.Number
	)

	if pe.Action != github.PullRequestActionOpened &&
		pe.Action != github.PullRequestActionReopened &&
		pe.Action != github.PullRequestActionSynchronize {
		return nil
	}

	changes, err := gc.GetPullRequestChanges(owner, repo, num)
	if err != nil {
		return fmt.Errorf("cannot get pull request changes for docs-no-retest plugin: %v", err)
	}

	docsOnly := true
	for _, change := range changes {
		_, basename := path.Split(change.Filename)
		if docFilesRegex.MatchString(basename) {
			continue
		}
		if ownersFilesRegex.MatchString(basename) {
			continue
		}
		if licenseFilesRegex.MatchString(basename) {
			continue
		}
		docsOnly = false
		break
	}

	labels, err := gc.GetIssueLabels(owner, repo, num)
	if err != nil {
		return fmt.Errorf("cannot get labels for docs-no-retest plugin: %v", err)
	}

	hasTargetLabel := false
	for _, label := range labels {
		if label.Name == labelSkipRetest {
			hasTargetLabel = true
			break
		}
	}

	if docsOnly && !hasTargetLabel {
		if err := gc.AddLabel(owner, repo, num, labelSkipRetest); err != nil {
			return fmt.Errorf("error adding label to %s/%s#%d: %v", owner, repo, num, err)
		}
	} else if !docsOnly && hasTargetLabel {
		if err := gc.RemoveLabel(owner, repo, num, labelSkipRetest); err != nil {
			return fmt.Errorf("error removing label from %s/%s#%d: %v", owner, repo, num, err)
		}
	}

	return nil
}
