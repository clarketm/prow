/*
Copyright 2018 The Kubernetes Authors.

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
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/pkg/io"
	"k8s.io/test-infra/prow/config"
	prowflagutil "k8s.io/test-infra/prow/flagutil"
	"k8s.io/test-infra/prow/gerrit/adapter"
	"k8s.io/test-infra/prow/gerrit/client"
	"k8s.io/test-infra/prow/interrupts"
	"k8s.io/test-infra/prow/logrusutil"
	"k8s.io/test-infra/prow/pjutil"
)

type options struct {
	gcsCredentialsFile string
	cookiefilePath     string
	configPath         string
	jobConfigPath      string
	projects           client.ProjectsFlag
	lastSyncFallback   string
	dryRun             bool
	kubernetes         prowflagutil.KubernetesOptions
}

func (o *options) Validate() error {
	if len(o.projects) == 0 {
		return errors.New("--gerrit-projects must be set")
	}

	if o.cookiefilePath == "" {
		logrus.Info("--cookiefile is not set, using anonymous authentication")
	}

	if o.configPath == "" {
		return errors.New("--config-path must be set")
	}

	if o.lastSyncFallback == "" {
		return errors.New("--last-sync-fallback must be set")
	}

	if strings.HasPrefix(o.lastSyncFallback, "gs://") && o.gcsCredentialsFile == "" {
		logrus.WithField("last-sync-fallback", o.lastSyncFallback).Warn("--gcs-credentials-file unset, will try and access with a default service account")
	}
	return nil
}

func gatherOptions(fs *flag.FlagSet, args ...string) options {
	var o options
	o.projects = client.ProjectsFlag{}
	fs.StringVar(&o.configPath, "config-path", "", "Path to config.yaml.")
	fs.StringVar(&o.jobConfigPath, "job-config-path", "", "Path to prow job configs")
	fs.StringVar(&o.cookiefilePath, "cookiefile", "", "Path to git http.cookiefile, leave empty for anonymous")
	fs.Var(&o.projects, "gerrit-projects", "Set of gerrit repos to monitor on a host example: --gerrit-host=https://android.googlesource.com=platform/build,toolchain/llvm, repeat fs for each host")
	fs.StringVar(&o.lastSyncFallback, "last-sync-fallback", "", "Local or gs:// path to sync the latest timestamp")
	fs.StringVar(&o.gcsCredentialsFile, "gcs-credentials-file", "", "Path to GCS credentials. Required for a --last-sync-fallback=gs://path")
	fs.BoolVar(&o.dryRun, "dry-run", false, "Run in dry-run mode, performing no modifying actions.")
	o.kubernetes.AddFlags(fs)
	fs.Parse(args)
	return o
}

type syncTime struct {
	val    time.Time
	lock   sync.RWMutex
	path   string
	opener io.Opener
	ctx    context.Context
}

func (st *syncTime) init() error {
	fmt.Println(st.val)
	st.lock.RLock()
	zero := st.val.IsZero()
	st.lock.RUnlock()
	if !zero {
		return nil
	}
	st.lock.Lock()
	defer st.lock.Unlock()
	if !st.val.IsZero() {
		return nil // Someone else set it while we waited for the write lock
	}
	unix, err := st.currentInt()
	if err != nil {
		return err
	}
	if unix == 0 {
		st.val = time.Now()
		logrus.Warnf("Reset lastSyncFallback to %v", st.val)
	} else {
		st.val = time.Unix(unix, 0)
	}
	return nil
}

func (st *syncTime) currentInt() (int64, error) {
	r, err := st.opener.Reader(st.ctx, st.path)
	if io.IsNotExist(err) {
		logrus.Warnf("lastSyncFallback not found at %q", st.path)
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("open: %v", err)
	}
	defer io.LogClose(r)
	buf, err := ioutil.ReadAll(r)
	if err != nil {
		return 0, fmt.Errorf("read: %v", err)
	}
	unix, err := strconv.ParseInt(string(buf), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse int: %v", err)
	}
	return unix, nil
}

func (st *syncTime) Current() time.Time {
	st.lock.RLock()
	defer st.lock.RUnlock()
	return st.val
}

func (st *syncTime) Update(t time.Time) error {
	st.lock.Lock()
	defer st.lock.Unlock()
	if !t.After(st.val) {
		return nil
	}
	w, err := st.opener.Writer(st.ctx, st.path)
	if err != nil {
		return fmt.Errorf("open for write %q: %v", st.path, err)
	}
	lastSyncUnix := strconv.FormatInt(t.Unix(), 10)
	if _, err := fmt.Fprint(w, lastSyncUnix); err != nil {
		io.LogClose(w)
		return fmt.Errorf("write %q: %v", st.path, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close %q: %v", st.path, err)
	}
	st.val = t
	return nil
}

func main() {
	logrusutil.ComponentInit("gerrit")

	defer interrupts.WaitForGracefulShutdown()

	pjutil.ServePProf()

	o := gatherOptions(flag.NewFlagSet(os.Args[0], flag.ExitOnError), os.Args[1:]...)
	if err := o.Validate(); err != nil {
		logrus.Fatalf("Invalid options: %v", err)
	}

	ca := &config.Agent{}
	if err := ca.Start(o.configPath, o.jobConfigPath); err != nil {
		logrus.WithError(err).Fatal("Error starting config agent.")
	}
	cfg := ca.Config

	prowJobClient, err := o.kubernetes.ProwJobClient(cfg().ProwJobNamespace, o.dryRun)
	if err != nil {
		logrus.WithError(err).Fatal("Error getting kube client.")
	}

	ctx := context.Background() // TODO(fejta): use something better
	op, err := io.NewOpener(ctx, o.gcsCredentialsFile)
	if err != nil {
		logrus.WithError(err).Fatal("Error creating opener")
	}
	st := syncTime{
		path:   o.lastSyncFallback,
		ctx:    ctx,
		opener: op,
	}
	if err := st.init(); err != nil {
		logrus.WithError(err).Fatal("Error initializing lastSyncFallback.")
	}
	c, err := adapter.NewController(&st, o.cookiefilePath, o.projects, prowJobClient, cfg)
	if err != nil {
		logrus.WithError(err).Fatal("Error creating gerrit client.")
	}

	logrus.Infof("Starting gerrit fetcher")

	interrupts.Tick(func() {
		start := time.Now()
		if err := c.Sync(); err != nil {
			logrus.WithError(err).Error("Error syncing.")
		}
		logrus.WithField("duration", fmt.Sprintf("%v", time.Since(start))).Info("Synced")
	}, func() time.Duration {
		return cfg().Gerrit.TickInterval.Duration
	})
}
