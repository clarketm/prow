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

package close

import (
	"fmt"
	"regexp"

	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/plugins"
)

const pluginName = "close"

var closeRe = regexp.MustCompile(`(?mi)^/close\s*$`)

func init() {
	plugins.RegisterGenericCommentHandler(pluginName, handleGenericComment)
}

type githubClient interface {
	CreateComment(owner, repo string, number int, comment string) error
	CloseIssue(owner, repo string, number int) error
	ClosePR(owner, repo string, number int) error
	IsMember(owner, login string) (bool, error)
	AssignIssue(owner, repo string, number int, assignees []string) error
}

func handleGenericComment(pc plugins.PluginClient, e github.GenericCommentEvent) error {
	return handle(pc.GitHubClient, pc.Logger, &e)
}

func handle(gc githubClient, log *logrus.Entry, e *github.GenericCommentEvent) error {
	// Only consider open issues and new comments.
	if e.IssueState != "open" || e.Action != github.GenericCommentActionCreated {
		return nil
	}

	if !closeRe.MatchString(e.Body) {
		return nil
	}

	org := e.Repo.Owner.Login
	repo := e.Repo.Name
	number := e.Number
	commentAuthor := e.User.Login

	// Allow assignees and authors to close issues.
	isAssignee := false
	for _, assignee := range e.Assignees {
		if commentAuthor == assignee.Login {
			isAssignee = true
			break
		}
	}
	if e.IssueAuthor.Login != commentAuthor && !isAssignee {
		log.Infof("Assigning %s/%s#%d to %s", org, repo, number, commentAuthor)
		if err := gc.AssignIssue(org, repo, number, []string{commentAuthor}); err != nil {
			msg := "Assigning you to the issue failed."
			if ok, merr := gc.IsMember(org, commentAuthor); merr == nil && !ok {
				msg = "Only kubernetes org members may be assigned issues."
			} else if merr != nil {
				log.WithError(merr).Errorf("Failed IsMember(%s, %s)", org, commentAuthor)
			} else {
				log.WithError(err).Errorf("Failed AssignIssue(%s, %s, %d, %s)", org, repo, number, commentAuthor)
			}
			resp := fmt.Sprintf("you can't close an issue unless you authored it or you are assigned to it, %s.", msg)
			log.Infof("Commenting \"%s\".", resp)
			return gc.CreateComment(org, repo, number, plugins.FormatResponseRaw(e.Body, e.HTMLURL, commentAuthor, resp))
		}
	}

	if e.IsPR {
		log.Info("Closing PR.")
		return gc.ClosePR(org, repo, number)
	}

	log.Info("Closing issue.")
	return gc.CloseIssue(org, repo, number)
}
