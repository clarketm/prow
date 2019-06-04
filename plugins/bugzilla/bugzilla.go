/*
Copyright 2019 The Kubernetes Authors.

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

// Package bugzilla ensures that pull requests reference a Bugzilla bug in their title
package bugzilla

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/bugzilla"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/labels"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/plugins"
)

var (
	titleMatch   = regexp.MustCompile(`(?i)^.*Bug ([0-9]+):`)
	commandMatch = regexp.MustCompile(`(?mi)^/bugzilla refresh\s*$`)
)

const (
	PluginName = "bugzilla"
	bugLink    = `[Bugzilla bug](%s/show_bug.cgi?id=%d)`
)

func init() {
	plugins.RegisterGenericCommentHandler(PluginName, handleGenericComment, helpProvider)
	plugins.RegisterPullRequestHandler(PluginName, handlePullRequest, helpProvider)
}

func helpProvider(config *plugins.Configuration, enabledRepos []string) (*pluginhelp.PluginHelp, error) {
	configInfo := make(map[string]string)
	for _, orgRepo := range enabledRepos {
		parts := strings.Split(orgRepo, "/")
		var opts map[string]plugins.BugzillaBranchOptions
		switch len(parts) {
		case 2:
			opts = config.Bugzilla.OptionsForRepo(parts[0], parts[1])
		default:
			return nil, fmt.Errorf("invalid repo in enabledRepos: %q", orgRepo)
		}
		if len(opts) == 0 {
			continue
		}
		// we need to make sure the order of this help is consistent for page reloads and testing
		var branches []string
		for branch := range opts {
			branches = append(branches, branch)
		}
		sort.Strings(branches)
		var configInfoStrings []string
		configInfoStrings = append(configInfoStrings, "The plugin has the following configuration:<ul>")
		for _, branch := range branches {
			var message string
			if branch == plugins.BugzillaOptionsWildcard {
				message = "by default, "
			} else {
				message = fmt.Sprintf("on the %q branch, ", branch)
			}
			message += "valid bugs must "
			var conditionsExist bool
			if opts[branch].IsOpen != nil {
				conditionsExist = true
				if *opts[branch].IsOpen {
					message += "be open"
				} else {
					message += "be closed"
				}
			}
			if opts[branch].TargetRelease != nil {
				conditionsExist = true
				if opts[branch].IsOpen != nil {
					message += " and "
				}
				message += fmt.Sprintf("target the %q release", *opts[branch].TargetRelease)
			}
			if !conditionsExist {
				message += "exist"
			}
			configInfoStrings = append(configInfoStrings, "<li>"+message+"</li>")
		}
		configInfoStrings = append(configInfoStrings, fmt.Sprintf("</ul>"))
		configInfo[orgRepo] = strings.Join(configInfoStrings, "\n")
	}
	pluginHelp := &pluginhelp.PluginHelp{
		Description: "The bugzilla plugin ensures that pull requests reference a valid Bugzilla bug in their title.",
		Config:      configInfo,
	}
	pluginHelp.AddCommand(pluginhelp.Command{
		Usage:       "/bugzilla refresh",
		Description: "Check Bugzilla for a valid bug referenced in the PR title",
		Featured:    false,
		WhoCanUse:   "Anyone",
		Examples:    []string{"/bugzilla refresh"},
	})
	return pluginHelp, nil
}

type githubClient interface {
	GetPullRequest(org, repo string, number int) (*github.PullRequest, error)
	CreateComment(owner, repo string, number int, comment string) error
	GetIssueLabels(org, repo string, number int) ([]github.Label, error)
	AddLabel(owner, repo string, number int, label string) error
	RemoveLabel(owner, repo string, number int, label string) error
}

func handleGenericComment(pc plugins.Agent, e github.GenericCommentEvent) error {
	event, err := digestComment(pc.GitHubClient, pc.Logger, e)
	if err != nil {
		return err
	}
	if event != nil {
		options := pc.PluginConfig.Bugzilla.OptionsForBranch(event.org, event.repo, event.baseRef)
		return handle(*event, pc.GitHubClient, pc.BugzillaClient, options, pc.Logger)
	}
	return nil
}

func handlePullRequest(pc plugins.Agent, pre github.PullRequestEvent) error {
	event, err := digestPR(pc.Logger, pre)
	if err != nil {
		return err
	}
	if event != nil {
		options := pc.PluginConfig.Bugzilla.OptionsForBranch(event.org, event.repo, event.baseRef)
		return handle(*event, pc.GitHubClient, pc.BugzillaClient, options, pc.Logger)
	}
	return nil
}

// digestPR determines if any action is necessary and creates the objects for handle() if it is
func digestPR(log *logrus.Entry, pre github.PullRequestEvent) (*event, error) {
	// These are the only actions indicating the PR title may have changed.
	if pre.Action != github.PullRequestActionOpened &&
		pre.Action != github.PullRequestActionReopened &&
		pre.Action != github.PullRequestActionEdited {
		return nil, nil
	}

	var (
		org     = pre.PullRequest.Base.Repo.Owner.Login
		repo    = pre.PullRequest.Base.Repo.Name
		baseRef = pre.PullRequest.Base.Ref
		number  = pre.PullRequest.Number
		title   = pre.PullRequest.Title
	)

	// Make sure the PR title is referencing a bug
	e := &event{org: org, repo: repo, baseRef: baseRef, number: number, body: title, htmlUrl: pre.PullRequest.HTMLURL, login: pre.PullRequest.User.Login}
	mat := titleMatch.FindStringSubmatch(title)
	if mat == nil {
		// in the case that the title used to reference a bug and no longer does we
		// want to handle this to remove labels
		e.missing = true
	} else {
		id, err := strconv.Atoi(mat[1])
		if err != nil {
			// should be impossible based on the regex
			log.WithError(err).Debug("Failed to parse bug ID as int - is the regex correct?")
			return nil, err
		}
		e.bugId = id
	}

	// when exiting early from errors trying to find out if the PR previously referenced a bug,
	// we want to handle the event only if a bug is currently referenced. Only when we know the
	// PR previously referenced a bug can we handle events where the PR currently does not
	var intermediate *event
	if !e.missing {
		intermediate = e
	}

	// Check if the previous version of the title referenced a bug.
	var changes struct {
		Title struct {
			From string `json:"from"`
		} `json:"title"`
	}
	if err := json.Unmarshal(pre.Changes, &changes); err != nil {
		// we're detecting this best-effort so we can handle it anyway
		return intermediate, nil
	}
	prevMat := titleMatch.FindStringSubmatch(changes.Title.From)
	if prevMat == nil {
		// title did not previously reference a bug
		return intermediate, nil
	}
	prevId, err := strconv.Atoi(prevMat[1])
	if err != nil {
		// should be impossible based on the regex, ignore err as this is best-effort
		log.WithError(err).Debug("Failed to parse bug ID as int - is the regex correct?")
		return intermediate, nil
	}

	// if the referenced bug has not changed in the update, ignore it
	if prevId == e.bugId {
		logrus.Debugf("Referenced Bugzilla ID (%d) has not changed, not handling event.", e.bugId)
		return nil, nil
	}

	// we know the PR previously referenced a bug, so whether
	// it currently does or does not reference a bug, we should
	// handle the event
	return e, nil
}

// digestComment determines if any action is necessary and creates the objects for handle() if it is
func digestComment(gc githubClient, log *logrus.Entry, gce github.GenericCommentEvent) (*event, error) {
	// Only consider new comments.
	if gce.Action != github.GenericCommentActionCreated {
		return nil, nil
	}
	// Make sure they are requesting a bug refresh
	if !commandMatch.MatchString(gce.Body) {
		return nil, nil
	}
	var (
		org    = gce.Repo.Owner.Login
		repo   = gce.Repo.Name
		number = gce.Number
	)

	// We don't support linking issues to Bugs
	if !gce.IsPR {
		log.Debug("Bug refresh requested on an issue, ignoring")
		return nil, gc.CreateComment(org, repo, number, plugins.FormatResponseRaw(gce.Body, gce.HTMLURL, gce.User.Login, `Bugzilla bug referencing is only supported for Pull Requests, not issues.`))
	}

	// Make sure the PR title is referencing a bug
	pr, err := gc.GetPullRequest(org, repo, number)
	if err != nil {
		return nil, err
	}

	e := &event{org: org, repo: repo, baseRef: pr.Base.Ref, number: number, body: gce.Body, htmlUrl: gce.HTMLURL, login: gce.User.Login}
	mat := titleMatch.FindStringSubmatch(pr.Title)
	if mat == nil {
		e.missing = true
		return e, nil
	}
	id, err := strconv.Atoi(mat[1])
	if err != nil {
		// should be impossible based on the regex
		log.WithError(err).Debug("Failed to parse bug ID as int - is the regex correct?")
		return nil, err
	}
	e.bugId = id

	return e, nil
}

type event struct {
	org, repo, baseRef   string
	number, bugId        int
	missing              bool
	body, htmlUrl, login string
}

func handle(e event, gc githubClient, bc bugzilla.Client, options plugins.BugzillaBranchOptions, log *logrus.Entry) error {
	comment := func(body string) error {
		return gc.CreateComment(e.org, e.repo, e.number, plugins.FormatResponseRaw(e.body, e.htmlUrl, e.login, body))
	}

	var needsValidLabel, needsInvalidLabel bool
	var response string
	if e.missing {
		log.WithField("bugMissing", true)
		log.Debug("No bug referenced.")
		needsValidLabel, needsInvalidLabel = false, false
		response = `No Bugzilla bug is referenced in the title of this pull request.
To reference a bug, add 'Bug XXX:' to the title of this pull request and request another bug refresh with <code>/bugzilla refresh</code>.`
	} else {
		log = log.WithField("bugId", e.bugId)

		bug, err := bc.GetBug(e.bugId)
		if err != nil && !bugzilla.IsNotFound(err) {
			log.WithError(err).Warn("Unexpected error searching for Bugzilla bug.")
			return comment(fmt.Sprintf(`An error was encountered searching the Bugzilla server at %s for bug %d:
> %v
Please contact an administrator to resolve this issue, then request a bug refresh with <code>/bugzilla refresh</code>.`,
				bc.Endpoint(), e.bugId, err))
		}
		if bugzilla.IsNotFound(err) || bug == nil {
			log.Debug("No bug found.")
			return comment(fmt.Sprintf(`No Bugzilla bug with ID %d exists in the tracker at %s.
Once a valid bug is referenced in the title of this pull request, request a bug refresh with <code>/bugzilla refresh</code>.`,
				e.bugId, bc.Endpoint()))
		}

		valid, why := validateBug(*bug, options)
		needsValidLabel, needsInvalidLabel = valid, !valid
		if valid {
			log.Debug("Valid bug found.")
			response = fmt.Sprintf(`This pull request references a valid `+bugLink+`.`, bc.Endpoint(), e.bugId)
		} else {
			log.Debug("Invalid bug found.")
			var formattedReasons string
			for _, reason := range why {
				formattedReasons += fmt.Sprintf(" - %s\n", reason)
			}
			response = fmt.Sprintf(`This pull request references an invalid `+bugLink+`:
%s`, bc.Endpoint(), e.bugId, formattedReasons)
		}
	}

	// ensure label state is correct. Do not propagate errors
	// as it is more important to report to the user than to
	// fail early on a label check.
	currentLabels, err := gc.GetIssueLabels(e.org, e.repo, e.number)
	if err != nil {
		log.WithError(err).Warn("Could not list labels on PR")
	}
	var hasValidLabel, hasInvalidLabel bool
	for _, l := range currentLabels {
		if l.Name == labels.ValidBug {
			hasValidLabel = true
		}
		if l.Name == labels.InvalidBug {
			hasInvalidLabel = true
		}
	}

	if needsValidLabel && !hasValidLabel {
		if err := gc.AddLabel(e.org, e.repo, e.number, labels.ValidBug); err != nil {
			log.WithError(err).Error("Failed to add valid bug label.")
		}
	} else if !needsValidLabel && hasValidLabel {
		if err := gc.RemoveLabel(e.org, e.repo, e.number, labels.ValidBug); err != nil {
			log.WithError(err).Error("Failed to remove valid bug label.")
		}
	}

	if needsInvalidLabel && !hasInvalidLabel {
		if err := gc.AddLabel(e.org, e.repo, e.number, labels.InvalidBug); err != nil {
			log.WithError(err).Error("Failed to add invalid bug label.")
		}
	} else if !needsInvalidLabel && hasInvalidLabel {
		if err := gc.RemoveLabel(e.org, e.repo, e.number, labels.InvalidBug); err != nil {
			log.WithError(err).Error("Failed to remove invalid bug label.")
		}
	}

	return comment(response)
}

// validateBug determines if the bug matches the options and returns a description of why not
func validateBug(bug bugzilla.Bug, options plugins.BugzillaBranchOptions) (bool, []string) {
	valid := true
	var errors []string
	if options.IsOpen != nil && *options.IsOpen != bug.IsOpen {
		valid = false
		not := ""
		was := "isn't"
		if !*options.IsOpen {
			not = "not "
			was = "is"
		}
		errors = append(errors, fmt.Sprintf("expected the bug to %sbe open, but it %s", not, was))
	}

	if options.TargetRelease != nil {
		if len(bug.TargetRelease) == 0 {
			valid = false
			errors = append(errors, fmt.Sprintf("expected the bug to target the %q release, but no target release was set", *options.TargetRelease))
		} else if *options.TargetRelease != bug.TargetRelease[0] {
			// the BugZilla web UI shows one option for target release, but returns the
			// field as a list in the REST API. We only care for the first item and it's
			// not even clear if the list can have more than one item in the response
			valid = false
			errors = append(errors, fmt.Sprintf("expected the bug to target the %q release, but it targets %q instead", *options.TargetRelease, bug.TargetRelease[0]))
		}
	}

	return valid, errors
}
