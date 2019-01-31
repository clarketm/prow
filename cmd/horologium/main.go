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

package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	prowapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/cron"
	"k8s.io/test-infra/prow/flagutil"
	"k8s.io/test-infra/prow/logrusutil"
	"k8s.io/test-infra/prow/pjutil"
)

type options struct {
	configPath    string
	jobConfigPath string

	kubernetes flagutil.KubernetesOptions
	dryRun     bool
}

func gatherOptions() options {
	o := options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	fs.StringVar(&o.configPath, "config-path", "/etc/config/config.yaml", "Path to config.yaml.")
	fs.StringVar(&o.jobConfigPath, "job-config-path", "", "Path to prow job configs.")

	fs.BoolVar(&o.dryRun, "dry-run", true, "Whether or not to make mutating API calls to GitHub.")
	o.kubernetes.AddFlags(fs)

	fs.Parse(os.Args[1:])
	return o
}

func (o *options) Validate() error {
	if err := o.kubernetes.Validate(o.dryRun); err != nil {
		return err
	}

	return nil
}

func main() {
	o := gatherOptions()
	logrus.SetFormatter(
		logrusutil.NewDefaultFieldsFormatter(nil, logrus.Fields{"component": "horologium"}),
	)

	configAgent := config.Agent{}
	if err := configAgent.Start(o.configPath, o.jobConfigPath); err != nil {
		logrus.WithError(err).Fatal("Error starting config agent.")
	}

	prowJobClient, err := o.kubernetes.ProwJobClient(configAgent.Config().ProwJobNamespace, o.dryRun)
	if err != nil {
		logrus.WithError(err).Fatal("Error getting Kubernetes client.")
	}

	// start a cron
	cr := cron.New()
	cr.Start()

	for now := range time.Tick(1 * time.Minute) {
		start := time.Now()
		if err := sync(prowJobClient, configAgent.Config(), cr, now); err != nil {
			logrus.WithError(err).Error("Error syncing periodic jobs.")
		}
		logrus.Infof("Sync time: %v", time.Since(start))
	}
}

type prowJobClient interface {
	Create(*prowapi.ProwJob) (*prowapi.ProwJob, error)
	List(opts metav1.ListOptions) (*prowapi.ProwJobList, error)
}

type cronClient interface {
	SyncConfig(cfg *config.Config) error
	QueuedJobs() []string
}

func sync(prowJobClient prowJobClient, cfg *config.Config, cr cronClient, now time.Time) error {
	jobs, err := prowJobClient.List(metav1.ListOptions{LabelSelector: labels.Everything().String()})
	if err != nil {
		return fmt.Errorf("error listing prow jobs: %v", err)
	}
	latestJobs := pjutil.GetLatestProwJobs(jobs.Items, prowapi.PeriodicJob)

	if err := cr.SyncConfig(cfg); err != nil {
		logrus.WithError(err).Error("Error syncing cron jobs.")
	}

	cronTriggers := map[string]bool{}
	for _, job := range cr.QueuedJobs() {
		cronTriggers[job] = true
	}

	var errs []error
	for _, p := range cfg.Periodics {
		j, ok := latestJobs[p.Name]

		if p.Cron == "" {
			if !ok || (j.Complete() && now.Sub(j.Status.StartTime.Time) > p.GetInterval()) {
				prowJob := pjutil.NewProwJob(pjutil.PeriodicSpec(p), p.Labels)
				if _, err := prowJobClient.Create(&prowJob); err != nil {
					errs = append(errs, err)
				}
			}
		} else if _, exist := cronTriggers[p.Name]; exist {
			if !ok || j.Complete() {
				prowJob := pjutil.NewProwJob(pjutil.PeriodicSpec(p), p.Labels)
				if _, err := prowJobClient.Create(&prowJob); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to create %d prowjobs: %v", len(errs), errs)
	}

	return nil
}
