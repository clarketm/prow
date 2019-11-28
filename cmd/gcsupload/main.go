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

// gcsupload uploads the files and folders specified
// to GCS using the Prow-defined job configuration
package main

import (
	"github.com/sirupsen/logrus"
	"github.com/clarketm/prow/pod-utils/downwardapi"
	"github.com/clarketm/prow/pod-utils/options"

	"github.com/clarketm/prow/gcsupload"
	"github.com/clarketm/prow/logrusutil"
	"github.com/clarketm/prow/pod-utils/gcs"
)

func main() {
	logrusutil.ComponentInit("gcsupload")

	o := gcsupload.NewOptions()
	if err := options.Load(o); err != nil {
		logrus.Fatalf("Could not resolve options: %v", err)
	}

	if err := o.Validate(); err != nil {
		logrus.Fatalf("Invalid options: %v", err)
	}

	spec, err := downwardapi.ResolveSpecFromEnv()
	if err != nil {
		logrus.WithError(err).Fatal("Could not resolve job spec")
	}

	if err := o.Run(spec, map[string]gcs.UploadFunc{}); err != nil {
		logrus.WithError(err).Fatal("Failed to upload to GCS")
	}
}
