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

package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	prowapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	prowv1 "k8s.io/test-infra/prow/client/clientset/versioned/typed/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/flagutil"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/logrusutil"
	"k8s.io/test-infra/prow/pjutil"
)

type options struct {
	runOnce       bool
	configPath    string
	jobConfigPath string
	dryRun        flagutil.Bool
	kubernetes    flagutil.ExperimentalKubernetesOptions
}

const (
	// TODO(fejta): require setting this explicitly
	defaultConfigPath = "/etc/config/config.yaml"

	reasonAged     = "aged"
	reasonOrphaned = "orphaned"
)

func gatherOptions(fs *flag.FlagSet, args ...string) options {
	o := options{}
	fs.BoolVar(&o.runOnce, "run-once", false, "If true, run only once then quit.")
	fs.StringVar(&o.configPath, "config-path", defaultConfigPath, "Path to config.yaml.")
	fs.StringVar(&o.jobConfigPath, "job-config-path", "", "Path to prow job configs.")

	// TODO(fejta): switch dryRun to be a bool, defaulting to true after March 15, 2019.
	fs.Var(&o.dryRun, "dry-run", "Whether or not to make mutating API calls to Kubernetes.")

	o.kubernetes.AddFlags(fs)
	fs.Parse(args)
	return o
}

func (o *options) Validate() error {
	if err := o.kubernetes.Validate(o.dryRun.Value); err != nil {
		return err
	}

	if o.configPath == "" {
		return errors.New("--config-path is required")
	}

	return nil
}

func main() {
	o := gatherOptions(flag.NewFlagSet(os.Args[0], flag.ExitOnError), os.Args[1:]...)
	if err := o.Validate(); err != nil {
		logrus.WithError(err).Fatal("Invalid options")
	}

	pjutil.ServePProf()

	logrus.SetFormatter(
		logrusutil.NewDefaultFieldsFormatter(nil, logrus.Fields{"component": "sinker"}),
	)
	if !o.dryRun.Explicit {
		logrus.Warning("Sinker requies --dry-run=false to function correctly in production.")
		logrus.Warning("--dry-run will soon default to true. Set --dry-run=false by March 15.")
	}

	configAgent := &config.Agent{}
	if err := configAgent.Start(o.configPath, o.jobConfigPath); err != nil {
		logrus.WithError(err).Fatal("Error starting config agent.")
	}
	cfg := configAgent.Config

	prowJobClient, err := o.kubernetes.ProwJobClient(cfg().ProwJobNamespace, o.dryRun.Value)
	if err != nil {
		logrus.WithError(err).Fatal("Error creating ProwJob client.")
	}

	buildClusterClients, err := o.kubernetes.BuildClusterClients(cfg().PodNamespace, o.dryRun.Value)
	if err != nil {
		logrus.WithError(err).Fatal("Error creating build cluster clients.")
	}

	var podClients []corev1.PodInterface
	for _, client := range buildClusterClients {
		// sinker doesn't care about build cluster aliases
		podClients = append(podClients, client)
	}

	c := controller{
		logger:        logrus.NewEntry(logrus.StandardLogger()),
		prowJobClient: prowJobClient,
		podClients:    podClients,
		config:        cfg,
	}

	// Clean now and regularly from now on.
	for {
		start := time.Now()
		c.clean()
		logrus.Infof("Sync time: %v", time.Since(start))
		if o.runOnce {
			break
		}
		time.Sleep(cfg().Sinker.ResyncPeriod)
	}
}

type controller struct {
	logger        *logrus.Entry
	prowJobClient prowv1.ProwJobInterface
	podClients    []corev1.PodInterface
	config        config.Getter
}

type sinkerReconciliationMetrics struct {
	podsCreated      int
	startAt          time.Time
	finishedAt       time.Time
	podsRemoved      map[string]int
	podRemovalErrors map[string]int
}

func (m *sinkerReconciliationMetrics) getPodsTotalRemoved() int {
	result := 0
	for _, v := range m.podsRemoved {
		result += v
	}
	return result
}

func (m *sinkerReconciliationMetrics) logFields() logrus.Fields {
	return logrus.Fields{
		"podsCreated":      m.podsCreated,
		"timeUsed":         m.finishedAt.Sub(m.startAt),
		"podsTotalRemoved": m.getPodsTotalRemoved(),
		"podsRemoved":      m.podsRemoved,
		"podRemovalErrors": m.podRemovalErrors,
	}
}

func (c *controller) clean() {

	metrics := sinkerReconciliationMetrics{startAt: time.Now(), podsRemoved: map[string]int{},
		podRemovalErrors: map[string]int{}}

	// Clean up old prow jobs first.
	prowJobs, err := c.prowJobClient.List(metav1.ListOptions{})
	if err != nil {
		c.logger.WithError(err).Error("Error listing prow jobs.")
		return
	}

	// Only delete pod if its prowjob is marked as finished
	isExist := sets.NewString()
	isFinished := sets.NewString()

	maxProwJobAge := c.config().Sinker.MaxProwJobAge
	for _, prowJob := range prowJobs.Items {
		isExist.Insert(prowJob.ObjectMeta.Name)
		// Handle periodics separately.
		if prowJob.Spec.Type == prowapi.PeriodicJob {
			continue
		}
		if !prowJob.Complete() {
			continue
		}
		isFinished.Insert(prowJob.ObjectMeta.Name)
		if time.Since(prowJob.Status.StartTime.Time) <= maxProwJobAge {
			continue
		}
		if err := c.prowJobClient.Delete(prowJob.ObjectMeta.Name, &metav1.DeleteOptions{}); err == nil {
			c.logger.WithFields(pjutil.ProwJobFields(&prowJob)).Info("Deleted prowjob.")
		} else {
			c.logger.WithFields(pjutil.ProwJobFields(&prowJob)).WithError(err).Error("Error deleting prowjob.")
		}
	}

	// Keep track of what periodic jobs are in the config so we will
	// not clean up their last prowjob.
	isActivePeriodic := make(map[string]bool)
	for _, p := range c.config().Periodics {
		isActivePeriodic[p.Name] = true
	}

	// Get the jobs that we need to retain so horologium can continue working
	// as intended.
	latestPeriodics := pjutil.GetLatestProwJobs(prowJobs.Items, prowapi.PeriodicJob)
	for _, prowJob := range prowJobs.Items {
		if prowJob.Spec.Type != prowapi.PeriodicJob {
			continue
		}

		latestPJ := latestPeriodics[prowJob.Spec.Job]
		if isActivePeriodic[prowJob.Spec.Job] && prowJob.ObjectMeta.Name == latestPJ.ObjectMeta.Name {
			// Ignore deleting this one.
			continue
		}
		if !prowJob.Complete() {
			continue
		}
		isFinished.Insert(prowJob.ObjectMeta.Name)
		if time.Since(prowJob.Status.StartTime.Time) <= maxProwJobAge {
			continue
		}
		if err := c.prowJobClient.Delete(prowJob.ObjectMeta.Name, &metav1.DeleteOptions{}); err == nil {
			c.logger.WithFields(pjutil.ProwJobFields(&prowJob)).Info("Deleted prowjob.")
		} else {
			c.logger.WithFields(pjutil.ProwJobFields(&prowJob)).WithError(err).Error("Error deleting prowjob.")
		}
	}

	// Now clean up old pods.
	selector := fmt.Sprintf("%s = %s", kube.CreatedByProw, "true")
	for _, client := range c.podClients {
		pods, err := client.List(metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			c.logger.WithError(err).Error("Error listing pods.")
			return
		}
		metrics.podsCreated = len(pods.Items)
		maxPodAge := c.config().Sinker.MaxPodAge
		for _, pod := range pods.Items {
			clean := !pod.Status.StartTime.IsZero() && time.Since(pod.Status.StartTime.Time) > maxPodAge
			reason := reasonAged
			if !isFinished.Has(pod.ObjectMeta.Name) {
				// prowjob exists and is not marked as completed yet
				// deleting the pod now will result in plank creating a brand new pod
				clean = false
			}
			if !isExist.Has(pod.ObjectMeta.Name) {
				// prowjob has gone, we want to clean orphan pods regardless of the state
				reason = reasonOrphaned
				clean = true
			}

			if !clean {
				continue
			}

			// Delete old finished or orphan pods. Don't quit if we fail to delete one.
			if err := client.Delete(pod.ObjectMeta.Name, &metav1.DeleteOptions{}); err == nil {
				c.logger.WithField("pod", pod.ObjectMeta.Name).Info("Deleted old completed pod.")
				metrics.podsRemoved[reason]++
			} else {
				c.logger.WithField("pod", pod.ObjectMeta.Name).WithError(err).Error("Error deleting pod.")
				metrics.podRemovalErrors[err.Error()]++
			}
		}
	}

	metrics.finishedAt = time.Now()
	c.logger.WithFields(metrics.logFields()).Info("Sinker reconciliation complete.")
}
