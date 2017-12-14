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

package lifecycle

import (
	"fmt"
	"regexp"
	"time"

	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/plugins"
)

const deprecatedWarn = true

var (
	deprecatedTick = time.Tick(time.Hour) // Warn once per hour
	lifecycleRe    = regexp.MustCompile(`(?mi)^/(remove-)?lifecycle (frozen|stale|putrid|rotten)\s*$`)
)

func init() {
	plugins.RegisterGenericCommentHandler("lifecycle", lifecycleHandleGenericComment, help)
	logrus.SetLevel(logrus.DebugLevel)
}

func help(config *plugins.Configuration, enabledRepos []string) (*pluginhelp.PluginHelp, error) {
	return &pluginhelp.PluginHelp{
		Description: "Close, reopen, flag and/or unflag an issue or PR as stale/putrid/rotten/frozen",
		WhoCanUse:   "Authors and assignees can close/reopen. Anyone can flag/unflag",
		Usage:       "/close\n/reopen\n/lifecycle <frozen|stale|putrid|rotten>\n/remove-lifecycle <frozen|stale|putrid|rotten",
		Examples:    []string{"/close", "/reopen", "/lifecycle frozen", "/remove-lifecycle stale"},
	}, nil
}

type commentClient interface {
	CreateComment(owner, repo string, number int, comment string) error
}

type lifecycleClient interface {
	CreateComment(owner, repo string, number int, comment string) error
	AddLabel(owner, repo string, number int, label string) error
	RemoveLabel(owner, repo string, number int, label string) error
}

func deprecate(gc commentClient, plugin, org, repo string, number int, e *github.GenericCommentEvent) error {
	select {
	case <-deprecatedTick:
		// Only warn once per tick
		return gc.CreateComment(org, repo, number, plugins.FormatResponseRaw(e.Body, e.HTMLURL, e.User.Login, fmt.Sprintf("The %s prow plugin is deprecated, please migrate to the lifecycle plugin before April 2018", plugin)))
	default:
		return nil
	}
}

func lifecycleHandleGenericComment(pc plugins.PluginClient, e github.GenericCommentEvent) error {
	gc := pc.GitHubClient
	log := pc.Logger
	if err := handleReopen(gc, log, &e, !deprecatedWarn); err != nil {
		return err
	}
	if err := handleClose(gc, log, &e, !deprecatedWarn); err != nil {
		return err
	}
	return handle(gc, log, &e)
}

func handle(gc lifecycleClient, log *logrus.Entry, e *github.GenericCommentEvent) error {
	// Only consider new comments.
	if e.Action != github.GenericCommentActionCreated {
		return nil
	}

	for _, mat := range lifecycleRe.FindAllStringSubmatch(e.Body, -1) {
		if err := handleOne(gc, log, e, mat); err != nil {
			return err
		}
	}
	return nil
}

func handleOne(gc lifecycleClient, log *logrus.Entry, e *github.GenericCommentEvent, mat []string) error {
	org := e.Repo.Owner.Login
	repo := e.Repo.Name
	number := e.Number

	remove := mat[1] != ""
	cmd := mat[2]
	lbl := "lifecycle/" + cmd
	// Let's start simple and allow anyone to add/remove frozen, stale, putrid, rotten labels.
	// Adjust if we find evidence of the community abusing these labels.
	if remove {
		log.Infof("/remove-%s", cmd)
		return gc.RemoveLabel(org, repo, number, lbl)
	}
	log.Infof("/%s", cmd)
	return gc.AddLabel(org, repo, number, lbl)
}
