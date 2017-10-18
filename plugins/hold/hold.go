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

// Package hold contains a plugin which will allow users to label their
// own pull requests as not ready or ready for merge. The submit queue
// will honor the label to ensure pull requests do not merge when it is
// applied.
package hold

import (
	"fmt"
	"regexp"

	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/plugins"
)

const pluginName = "hold"

var (
	label         = "do-not-merge/hold"
	labelRe       = regexp.MustCompile(`(?mi)^/hold\s*$`)
	labelCancelRe = regexp.MustCompile(`(?mi)^/hold cancel\s*$`)
)

type hasLabelFunc func(e *github.GenericCommentEvent) (bool, error)

func init() {
	plugins.RegisterGenericCommentHandler(pluginName, handleGenericComment)
}

type githubClient interface {
	AddLabel(owner, repo string, number int, label string) error
	RemoveLabel(owner, repo string, number int, label string) error
	GetIssueLabels(org, repo string, number int) ([]github.Label, error)
}

func handleGenericComment(pc plugins.PluginClient, e github.GenericCommentEvent) error {
	hasLabel := func(e *github.GenericCommentEvent) (bool, error) {
		return hasLabel(pc.GitHubClient, e.Repo.Owner.Login, e.Repo.Name, e.Number, label)
	}
	return handle(pc.GitHubClient, pc.Logger, &e, hasLabel)
}

// handle drives the pull request to the desired state. If any user adds
// a /hold directive, we want to add a label if one does not already exist.
// If they add /hold cancel, we want to remove the label if it exists.
func handle(gc githubClient, log *logrus.Entry, e *github.GenericCommentEvent, f hasLabelFunc) error {
	if e.Action != github.GenericCommentActionCreated {
		return nil
	}
	needsLabel := false
	if labelRe.MatchString(e.Body) {
		needsLabel = true
	} else if labelCancelRe.MatchString(e.Body) {
		needsLabel = false
	} else {
		return nil
	}

	hasLabel, err := f(e)
	if err != nil {
		return err
	}

	org := e.Repo.Owner.Login
	repo := e.Repo.Name

	if hasLabel && !needsLabel {
		log.Info("Removing %q label for %s/%s#%d", label, org, repo, e.Number)
		return gc.RemoveLabel(org, repo, e.Number, label)
	} else if !hasLabel && needsLabel {
		log.Info("Adding %q label for %s/%s#%d", label, org, repo, e.Number)
		return gc.AddLabel(org, repo, e.Number, label)
	}
	return nil
}

// hasLabel checks if a label is applied to a pr.
func hasLabel(c githubClient, org, repo string, num int, label string) (bool, error) {
	labels, err := c.GetIssueLabels(org, repo, num)
	if err != nil {
		return false, fmt.Errorf("failed to get the labels on %s/%s#%d: %v", org, repo, num, err)
	}
	for _, candidate := range labels {
		if candidate.Name == label {
			return true, nil
		}
	}
	return false, nil
}
