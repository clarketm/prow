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

package updateconfig

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"path"

	zglob "github.com/mattn/go-zglob"
	"github.com/sirupsen/logrus"
	coreapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/plugins"
)

const (
	pluginName = "config-updater"
)

func init() {
	plugins.RegisterPullRequestHandler(pluginName, handlePullRequest, helpProvider)
}

func helpProvider(config *plugins.Configuration, enabledRepos []string) (*pluginhelp.PluginHelp, error) {
	var configInfo map[string]string
	if len(enabledRepos) == 1 {
		msg := fmt.Sprintf(
			"The main configuration is kept in sync with '%s/%s'.\nThe plugin configuration is kept in sync with '%s/%s'.",
			enabledRepos[0],
			config.ConfigUpdater.ConfigFile,
			enabledRepos[0],
			config.ConfigUpdater.PluginFile,
		)
		configInfo = map[string]string{"": msg}
	}
	return &pluginhelp.PluginHelp{
			Description: "The config-updater plugin automatically redeploys configuration and plugin configuration files when they change. The plugin watches for pull request merges that modify either of the config files and updates the cluster's configmap resources in response.",
			Config:      configInfo,
		},
		nil
}

type githubClient interface {
	CreateComment(owner, repo string, number int, comment string) error
	GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error)
	GetFile(org, repo, filepath, commit string) ([]byte, error)
}

func handlePullRequest(pc plugins.Agent, pre github.PullRequestEvent) error {
	return handle(pc.GitHubClient, pc.KubernetesClient.CoreV1(), pc.Config.ProwJobNamespace, pc.Logger, pre, &pc.PluginConfig.ConfigUpdater)
}

// FileGetter knows how to get the contents of a file by name
type FileGetter interface {
	GetFile(filename string) ([]byte, error)
}

type gitHubFileGetter struct {
	org, repo, commit string
	client            githubClient
}

func (g *gitHubFileGetter) GetFile(filename string) ([]byte, error) {
	return g.client.GetFile(g.org, g.repo, filename, g.commit)
}

// Update updates the configmap with the data from the identified files
func Update(fg FileGetter, kc corev1.ConfigMapInterface, name, namespace string, updates map[string]string, gzipContent bool, logger *logrus.Entry) error {
	cm, getErr := kc.Get(name, metav1.GetOptions{})
	isNotFound := errors.IsNotFound(getErr)
	if getErr != nil && !isNotFound {
		return fmt.Errorf("failed to fetch current state of configmap: %v", getErr)
	}

	if cm == nil {
		cm = &coreapi.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
		}
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}

	for key, filename := range updates {
		if filename == "" {
			logger.WithField("key", key).Debug("Deleting key.")
			delete(cm.Data, key)
			continue
		}

		content, err := fg.GetFile(filename)
		if err != nil {
			return fmt.Errorf("get file err: %v", err)
		}
		logger.WithFields(logrus.Fields{"key": key, "filename": filename}).Debug("Populating key.")
		value := string(content)
		if gzipContent {
			buff := bytes.NewBuffer([]byte{})
			// TODO: this error is wildly unlikely for anything that
			// would actually fit in a configmap, we could just as well return
			// the error instead of falling back to the raw content
			if _, err := gzip.NewWriter(buff).Write(content); err != nil {
				logger.WithError(err).Error("failed to gzip content, falling back to raw")
			} else {
				value = buff.String()
			}
		}
		cm.Data[key] = value
	}

	var updateErr error
	var verb string
	if getErr != nil && isNotFound {
		verb = "create"
		_, updateErr = kc.Create(cm)
	} else {
		verb = "update"
		_, updateErr = kc.Update(cm)
	}
	if updateErr != nil {
		return fmt.Errorf("%s config map err: %v", verb, updateErr)
	}
	return nil
}

// ConfigMapID is a name/namespace combination that identifies a config map
type ConfigMapID struct {
	Name, Namespace string
}

// FilterChanges determines which of the changes are relevant for config updating, returning mapping of
// config map to key to filename to update that key from.
func FilterChanges(configMaps map[string]plugins.ConfigMapSpec, changes []github.PullRequestChange, log *logrus.Entry) map[ConfigMapID]map[string]string {
	toUpdate := map[ConfigMapID]map[string]string{}
	for _, change := range changes {
		var cm plugins.ConfigMapSpec
		found := false

		for key, configMap := range configMaps {
			var matchErr error
			found, matchErr = zglob.Match(key, change.Filename)
			if matchErr != nil {
				// Should not happen, log matchErr and continue
				log.WithError(matchErr).Info("key matching error")
				continue
			}

			if found {
				cm = configMap
				break
			}
		}

		if !found {
			continue // This file does not define a configmap
		}

		// Yes, update the configmap with the contents of this file
		for _, ns := range append(cm.Namespaces) {
			id := ConfigMapID{Name: cm.Name, Namespace: ns}
			if _, ok := toUpdate[id]; !ok {
				toUpdate[id] = map[string]string{}
			}
			key := cm.Key
			if key == "" {
				key = path.Base(change.Filename)
				// if the key changed, we need to remove the old key
				if change.Status == github.PullRequestFileRenamed {
					oldKey := path.Base(change.PreviousFilename)
					toUpdate[id][oldKey] = ""
				}
			}
			if change.Status == github.PullRequestFileRemoved {
				toUpdate[id][key] = ""
			} else {
				toUpdate[id][key] = change.Filename
			}
		}
	}
	return toUpdate
}

func handle(gc githubClient, kc corev1.ConfigMapsGetter, defaultNamespace string, log *logrus.Entry, pre github.PullRequestEvent, config *plugins.ConfigUpdater) error {
	// Only consider newly merged PRs
	if pre.Action != github.PullRequestActionClosed {
		return nil
	}

	if len(config.Maps) == 0 { // Nothing to update
		return nil
	}

	pr := pre.PullRequest

	if !pr.Merged || pr.MergeSHA == nil || pr.Base.Repo.DefaultBranch != pr.Base.Ref {
		return nil
	}

	org := pr.Base.Repo.Owner.Login
	repo := pr.Base.Repo.Name

	// Which files changed in this PR?
	changes, err := gc.GetPullRequestChanges(org, repo, pr.Number)
	if err != nil {
		return err
	}

	message := func(name, namespace string, data map[string]string, indent string) string {
		identifier := fmt.Sprintf("`%s` configmap", name)
		if namespace != "" {
			identifier = fmt.Sprintf("%s in namespace `%s`", identifier, namespace)
		}
		msg := fmt.Sprintf("%s using the following files:", identifier)
		for key, file := range data {
			msg = fmt.Sprintf("%s\n%s- key `%s` using file `%s`", msg, indent, key, file)
		}
		return msg
	}

	// Are any of the changes files ones that define a configmap we want to update?
	toUpdate := FilterChanges(config.Maps, changes, log)

	var updated []string
	indent := " " // one space
	if len(toUpdate) > 1 {
		indent = "   " // three spaces for sub bullets
	}
	for cm, data := range toUpdate {
		if cm.Namespace == "" {
			cm.Namespace = defaultNamespace
		}
		logger := log.WithFields(logrus.Fields{"configmap": map[string]string{"name": cm.Name, "namespace": cm.Namespace}})
		if err := Update(&gitHubFileGetter{org: org, repo: repo, commit: *pr.MergeSHA, client: gc}, kc.ConfigMaps(cm.Namespace), cm.Name, cm.Namespace, data, config.GZIP, logger); err != nil {
			return err
		}
		updated = append(updated, message(cm.Name, cm.Namespace, data, indent))
	}

	var msg string
	switch n := len(updated); n {
	case 0:
		return nil
	case 1:
		msg = fmt.Sprintf("Updated the %s", updated[0])
	default:
		msg = fmt.Sprintf("Updated the following %d configmaps:\n", n)
		for _, updateMsg := range updated {
			msg += fmt.Sprintf(" * %s\n", updateMsg) // one space indent
		}
	}

	if err := gc.CreateComment(org, repo, pr.Number, plugins.FormatResponseRaw(pr.Body, pr.HTMLURL, pr.User.Login, msg)); err != nil {
		return fmt.Errorf("comment err: %v", err)
	}
	return nil
}
