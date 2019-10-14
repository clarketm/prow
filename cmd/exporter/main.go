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

package main

import (
	"flag"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"

	prowjobinformer "k8s.io/test-infra/prow/client/informers/externalversions"
	"k8s.io/test-infra/prow/config"
	prowflagutil "k8s.io/test-infra/prow/flagutil"
	"k8s.io/test-infra/prow/interrupts"
	"k8s.io/test-infra/prow/logrusutil"
	"k8s.io/test-infra/prow/metrics"
	"k8s.io/test-infra/prow/metrics/prowjobs"
	"k8s.io/test-infra/prow/pjutil"
)

type options struct {
	configPath string
	kubernetes prowflagutil.KubernetesOptions
}

func gatherOptions(fs *flag.FlagSet, args ...string) options {
	var o options

	fs.StringVar(&o.configPath, "config-path", "", "Path to config.yaml.")

	o.kubernetes.AddFlags(fs)
	if err := fs.Parse(os.Args[1:]); err != nil {
		logrus.WithError(err).Fatalf("cannot parse args: '%s'", os.Args[1:])
	}
	o.configPath = config.ConfigPath(o.configPath)
	return o
}

func (o *options) Validate() error {
	return o.kubernetes.Validate(false)
}

func mustRegister(component string, lister lister) *prometheus.Registry {
	registry := prometheus.NewRegistry()
	prometheus.WrapRegistererWith(prometheus.Labels{"collector_name": component}, registry).MustRegister(&prowJobCollector{
		lister: lister,
	})
	registry.MustRegister(
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
		prometheus.NewGoCollector(),
	)
	return registry
}

func main() {
	logrusutil.ComponentInit("exporter")
	o := gatherOptions(flag.NewFlagSet(os.Args[0], flag.ExitOnError), os.Args[1:]...)
	if err := o.Validate(); err != nil {
		logrus.WithError(err).Fatal("Invalid options")
	}

	defer interrupts.WaitForGracefulShutdown()

	pjutil.ServePProf()
	health := pjutil.NewHealth()

	configAgent := &config.Agent{}
	if err := configAgent.Start(o.configPath, ""); err != nil {
		logrus.WithError(err).Fatal("Error starting config agent.")
	}
	cfg := configAgent.Config

	pjClientset, err := o.kubernetes.ProwJobClientset(cfg().ProwJobNamespace, false)
	if err != nil {
		logrus.WithError(err).Fatal("Failed to create prowjob client set")
	}
	informerFactory := prowjobinformer.NewSharedInformerFactoryWithOptions(pjClientset, 0, prowjobinformer.WithNamespace(cfg().ProwJobNamespace))
	pjLister := informerFactory.Prow().V1().ProwJobs().Lister()

	prometheus.MustRegister(prowjobs.NewProwJobLifecycleHistogramVec(informerFactory.Prow().V1().ProwJobs().Informer()))

	go informerFactory.Start(interrupts.Context().Done())

	registry := mustRegister("exporter", pjLister)

	// Expose prometheus metrics
	metrics.ExposeMetricsWithRegistry("exporter", cfg().PushGateway, registry)

	logrus.Info("exporter is running ...")
	health.ServeReady()
}
