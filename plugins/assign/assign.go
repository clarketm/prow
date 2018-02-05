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

package assign

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/plugins"
)

const pluginName = "assign"

var (
	assignRe = regexp.MustCompile(`(?mi)^/(un)?assign(( @?[-\w]+?)*)\s*$`)

	CCRegexp = regexp.MustCompile(`(?mi)^/(un)?cc(( +@?[-\w]+?)*)\s*$`)
)

func init() {
	plugins.RegisterGenericCommentHandler(pluginName, handleGenericComment, helpProvider)
}

func helpProvider(config *plugins.Configuration, enabledRepos []string) (*pluginhelp.PluginHelp, error) {
	// The Config field is omitted because this plugin is not configurable.
	pluginHelp := &pluginhelp.PluginHelp{
		Description: "The assign plugin assigns or requests reviews from users. Specific users can be assigned with the command '/assign @user1' or have reviews requested of them with the command '/cc @user1'. If no user is specified the commands default to targetting the user who created the command. Assignments and requested reviews can be removed in the same way that they are added by prefixing the commands with 'un'.",
	}
	pluginHelp.AddCommand(pluginhelp.Command{
		Usage:       "/[un]assign [[@]<username>...]",
		Description: "Assigns an assignee to the PR",
		Featured:    true,
		WhoCanUse:   "Anyone can use the command, but the target user must be a member of the org that owns the repository.",
		Examples:    []string{"/assign", "/unassign", "/assign @k8s-ci-robot"},
	})
	pluginHelp.AddCommand(pluginhelp.Command{
		Usage:       "/[un]cc [[@]<username>...]",
		Description: "Requests a review from the user(s).",
		Featured:    true,
		WhoCanUse:   "Anyone can use the command, but the target user must be a member of the org that owns the repository.",
		Examples:    []string{"/cc", "/uncc", "/cc @k8s-ci-robot"},
	})
	return pluginHelp, nil
}

type githubClient interface {
	AssignIssue(owner, repo string, number int, logins []string) error
	UnassignIssue(owner, repo string, number int, logins []string) error

	RequestReview(org, repo string, number int, logins []string) error
	UnrequestReview(org, repo string, number int, logins []string) error

	CreateComment(owner, repo string, number int, comment string) error
}

func handleGenericComment(pc plugins.PluginClient, e github.GenericCommentEvent) error {
	if e.Action != github.GenericCommentActionCreated {
		return nil
	}
	err := handle(newAssignHandler(e, pc.GitHubClient, pc.Logger))
	if e.IsPR {
		err = combineErrors(err, handle(newReviewHandler(e, pc.GitHubClient, pc.Logger)))
	}
	return err
}

func parseLogins(text string) []string {
	var parts []string
	for _, p := range strings.Split(text, " ") {
		t := strings.Trim(p, "@ ")
		if t == "" {
			continue
		}
		parts = append(parts, t)
	}
	return parts
}

func combineErrors(err1, err2 error) error {
	if err1 != nil && err2 != nil {
		return fmt.Errorf("two errors: 1) %v 2) %v", err1, err2)
	} else if err1 != nil {
		return err1
	} else {
		return err2
	}
}

// handle is the generic handler for the assign plugin. It uses the handler's regexp and affectedLogins
// functions to identify the users to add and/or remove and then passes the appropriate users to the
// handler's add and remove functions. If add fails to add some of the users, a response comment is
// created where the body of the response is generated by the handler's addFailureResponse function.
func handle(h *handler) error {
	e := h.event
	org := e.Repo.Owner.Login
	repo := e.Repo.Name
	matches := h.regexp.FindAllStringSubmatch(e.Body, -1)
	if matches == nil {
		return nil
	}
	users := make(map[string]bool)
	for _, re := range matches {
		add := re[1] != "un" // un<cmd> == !add
		if re[2] == "" {
			users[e.User.Login] = add
		} else {
			for _, login := range parseLogins(re[2]) {
				users[login] = add
			}
		}
	}
	var toAdd, toRemove []string
	for login, add := range users {
		if add {
			toAdd = append(toAdd, login)
		} else {
			toRemove = append(toRemove, login)
		}
	}

	if len(toRemove) > 0 {
		h.log.Printf("Removing %s from %s/%s#%d: %v", h.userType, org, repo, e.Number, toRemove)
		if err := h.remove(org, repo, e.Number, toRemove); err != nil {
			return err
		}
	}
	if len(toAdd) > 0 {
		h.log.Printf("Adding %s to %s/%s#%d: %v", h.userType, org, repo, e.Number, toAdd)
		if err := h.add(org, repo, e.Number, toAdd); err != nil {
			if mu, ok := err.(github.MissingUsers); ok {
				msg := h.addFailureResponse(mu)
				if len(msg) == 0 {
					return nil
				}
				if err := h.gc.CreateComment(org, repo, e.Number, plugins.FormatResponseRaw(e.Body, e.HTMLURL, e.User.Login, msg)); err != nil {
					return fmt.Errorf("comment err: %v", err)
				}
				return nil
			}
			return err
		}
	}
	return nil
}

// handler is a struct that contains data about a github event and provides functions to help handle it.
type handler struct {
	// addFailureResponse generates the body of a response comment in the event that the add function fails.
	addFailureResponse func(mu github.MissingUsers) string
	// remove is the function that is called on the affected logins for a command prefixed with 'un'.
	remove func(org, repo string, number int, users []string) error
	// add is the function that is called on the affected logins for a command with no 'un' prefix.
	add func(org, repo string, number int, users []string) error

	// event is a pointer to the github.GenericCommentEvent struct that triggered the handler.
	event *github.GenericCommentEvent
	// regexp is the regular expression describing the command. It must have an optional 'un' prefix
	// as the first subgroup and the arguments to the command as the second subgroup.
	regexp *regexp.Regexp
	// gc is the githubClient to use for creating response comments in the event of a failure.
	gc githubClient

	// log is a logrus.Entry used to record actions the handler takes.
	log *logrus.Entry
	// userType is a string that represents the type of users affected by this handler. (e.g. 'assignees')
	userType string
}

func newAssignHandler(e github.GenericCommentEvent, gc githubClient, log *logrus.Entry) *handler {
	org := e.Repo.Owner.Login
	addFailureResponse := func(mu github.MissingUsers) string {
		return fmt.Sprintf("GitHub didn't allow me to assign the following users: %s.\n\nNote that only [%s members](https://github.com/orgs/%s/people) and repo collaborators can be assigned.", strings.Join(mu.Users, ", "), org, org)
	}

	return &handler{
		addFailureResponse: addFailureResponse,
		remove:             gc.UnassignIssue,
		add:                gc.AssignIssue,
		event:              &e,
		regexp:             assignRe,
		gc:                 gc,
		log:                log,
		userType:           "assignee(s)",
	}
}

func newReviewHandler(e github.GenericCommentEvent, gc githubClient, log *logrus.Entry) *handler {
	org := e.Repo.Owner.Login
	addFailureResponse := func(mu github.MissingUsers) string {
		return fmt.Sprintf("GitHub didn't allow me to request PR reviews from the following users: %s.\n\nNote that only [%s members](https://github.com/orgs/%s/people) and repo collaborators can review this PR, and authors cannot review their own PRs.", strings.Join(mu.Users, ", "), org, org)
	}

	return &handler{
		addFailureResponse: addFailureResponse,
		remove:             gc.UnrequestReview,
		add:                gc.RequestReview,
		event:              &e,
		regexp:             CCRegexp,
		gc:                 gc,
		log:                log,
		userType:           "reviewer(s)",
	}
}
