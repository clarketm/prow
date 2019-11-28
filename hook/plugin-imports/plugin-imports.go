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

package pluginimports

// We need to empty import all enabled plugins so that they will be linked into
// any hook binary.
import (
	_ "github.com/clarketm/prow/plugins/approve" // Import all enabled plugins.
	_ "github.com/clarketm/prow/plugins/assign"
	_ "github.com/clarketm/prow/plugins/blockade"
	_ "github.com/clarketm/prow/plugins/blunderbuss"
	_ "github.com/clarketm/prow/plugins/branchcleaner"
	_ "github.com/clarketm/prow/plugins/bugzilla"
	_ "github.com/clarketm/prow/plugins/buildifier"
	_ "github.com/clarketm/prow/plugins/cat"
	_ "github.com/clarketm/prow/plugins/cherrypickunapproved"
	_ "github.com/clarketm/prow/plugins/cla"
	_ "github.com/clarketm/prow/plugins/dco"
	_ "github.com/clarketm/prow/plugins/docs-no-retest"
	_ "github.com/clarketm/prow/plugins/dog"
	_ "github.com/clarketm/prow/plugins/golint"
	_ "github.com/clarketm/prow/plugins/goose"
	_ "github.com/clarketm/prow/plugins/heart"
	_ "github.com/clarketm/prow/plugins/help"
	_ "github.com/clarketm/prow/plugins/hold"
	_ "github.com/clarketm/prow/plugins/invalidcommitmsg"
	_ "github.com/clarketm/prow/plugins/label"
	_ "github.com/clarketm/prow/plugins/lgtm"
	_ "github.com/clarketm/prow/plugins/lifecycle"
	_ "github.com/clarketm/prow/plugins/mergecommitblocker"
	_ "github.com/clarketm/prow/plugins/milestone"
	_ "github.com/clarketm/prow/plugins/milestoneapplier"
	_ "github.com/clarketm/prow/plugins/milestonestatus"
	_ "github.com/clarketm/prow/plugins/override"
	_ "github.com/clarketm/prow/plugins/owners-label"
	_ "github.com/clarketm/prow/plugins/pony"
	_ "github.com/clarketm/prow/plugins/project"
	_ "github.com/clarketm/prow/plugins/projectmanager"
	_ "github.com/clarketm/prow/plugins/releasenote"
	_ "github.com/clarketm/prow/plugins/require-matching-label"
	_ "github.com/clarketm/prow/plugins/requiresig"
	_ "github.com/clarketm/prow/plugins/retitle"
	_ "github.com/clarketm/prow/plugins/shrug"
	_ "github.com/clarketm/prow/plugins/sigmention"
	_ "github.com/clarketm/prow/plugins/size"
	_ "github.com/clarketm/prow/plugins/skip"
	_ "github.com/clarketm/prow/plugins/slackevents"
	_ "github.com/clarketm/prow/plugins/stage"
	_ "github.com/clarketm/prow/plugins/trigger"
	_ "github.com/clarketm/prow/plugins/updateconfig"
	_ "github.com/clarketm/prow/plugins/verify-owners"
	_ "github.com/clarketm/prow/plugins/welcome"
	_ "github.com/clarketm/prow/plugins/wip"
	_ "github.com/clarketm/prow/plugins/yuks"
)
