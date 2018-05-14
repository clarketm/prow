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
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net/url"
	"strings"
	"sync"

	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/logrusutil"

	"github.com/sirupsen/logrus"
)

type options struct {
	config   string
	token    string
	confirm  bool
	endpoint string
}

func (o *options) Validate() error {
	if o.config == "" {
		return errors.New("empty --config")
	}

	if o.token == "" {
		return errors.New("empty --github-token-path")
	}

	if _, err := url.Parse(o.endpoint); err != nil {
		return fmt.Errorf("invalid --endpoint URL: %v", err)
	}

	return nil
}

func gatherOptions() options {
	o := options{}
	flag.StringVar(&o.config, "config-path", "", "Path to prow config.yaml")
	flag.BoolVar(&o.confirm, "confirm", false, "Mutate github if set")
	flag.StringVar(&o.endpoint, "github-endpoint", "https://api.github.com", "Github api endpoint, may differ for enterprise")
	flag.StringVar(&o.token, "github-token-path", "", "Path to github token")
	flag.Parse()
	return o
}

type Requirements struct {
	Org     string
	Repo    string
	Branch  string
	Request *github.BranchProtectionRequest
}

type Errors struct {
	lock sync.Mutex
	errs []error
}

func (e *Errors) add(err error) {
	e.lock.Lock()
	logrus.Info(err)
	defer e.lock.Unlock()
	e.errs = append(e.errs, err)
}

func main() {
	logrus.SetFormatter(
		logrusutil.NewDefaultFieldsFormatter(nil, logrus.Fields{"component": "branchprotector"}),
	)

	o := gatherOptions()
	if err := o.Validate(); err != nil {
		logrus.Fatal(err)
	}

	cfg, err := config.Load(o.config)
	if err != nil {
		logrus.WithError(err).Fatalf("Failed to load --config=%s", o.config)
	}

	b, err := ioutil.ReadFile(o.token)
	if err != nil {
		logrus.WithError(err).Fatalf("cannot read --token=%s", o.token)
	}

	var c *github.Client
	tok := strings.TrimSpace(string(b))
	if o.confirm {
		c = github.NewClient(tok, o.endpoint)
	} else {
		c = github.NewDryRunClient(tok, o.endpoint)
	}
	c.Throttle(300, 100) // 300 hourly tokens, bursts of 100

	p := Protector{
		client:         c,
		cfg:            cfg,
		updates:        make(chan Requirements),
		errors:         Errors{},
		completedRepos: make(map[string]bool),
		done:           make(chan []error),
	}

	go p.ConfigureBranches()
	p.Protect()
	close(p.updates)
	errors := <-p.done
	if n := len(errors); n > 0 {
		for i, err := range errors {
			logrus.WithError(err).Error(i)
		}
		logrus.Fatalf("Encountered %d errors protecting branches", n)
	}
}

type client interface {
	RemoveBranchProtection(org, repo, branch string) error
	UpdateBranchProtection(org, repo, branch string, config github.BranchProtectionRequest) error
	GetBranches(org, repo string) ([]github.Branch, error)
	GetRepos(org string, user bool) ([]github.Repo, error)
}

type Protector struct {
	client         client
	cfg            *config.Config
	updates        chan Requirements
	errors         Errors
	completedRepos map[string]bool
	done           chan []error
}

func (p *Protector) ConfigureBranches() {
	for u := range p.updates {
		if u.Request == nil {
			if err := p.client.RemoveBranchProtection(u.Org, u.Repo, u.Branch); err != nil {
				p.errors.add(fmt.Errorf("remove %s/%s=%s protection failed: %v", u.Org, u.Repo, u.Branch, err))
			}
			continue
		}

		if err := p.client.UpdateBranchProtection(u.Org, u.Repo, u.Branch, *u.Request); err != nil {
			p.errors.add(fmt.Errorf("update %s/%s=%s protection to %v failed: %v", u.Org, u.Repo, u.Branch, *u.Request, err))
		}
	}
	p.done <- p.errors.errs
}

// Protect branches specified in the presubmit and branch-protection config sections.
func (p *Protector) Protect() {
	bp := p.cfg.BranchProtection

	// Scan the branch-protection configuration
	for orgName, org := range bp.Orgs {
		if err := p.UpdateOrg(orgName, org, bp.HasProtect()); err != nil {
			p.errors.add(err)
		}
	}

	// Do not automatically protect tested repositories
	if !bp.ProtectTested {
		return
	}

	// Some repos with presubmits might not be listed in the branch-protection
	for repo := range p.cfg.Presubmits {
		if p.completedRepos[repo] == true {
			continue
		}
		parts := strings.Split(repo, "/")
		if len(parts) != 2 { // TODO(fejta): use a strong type here instead
			logrus.Fatalf("Bad repo: %s", repo)
		}
		orgName := parts[0]
		repoName := parts[1]
		if err := p.UpdateRepo(orgName, repoName, config.Repo{}); err != nil {
			p.errors.add(err)
		}
	}
}

// Update all repos in the org with the specified defaults
func (p *Protector) UpdateOrg(orgName string, org config.Org, allRepos bool) error {
	var repos []string
	allRepos = allRepos || org.HasProtect()
	if allRepos {
		// Strongly opinionated org, configure every repo in the org.
		rs, err := p.client.GetRepos(orgName, false)
		if err != nil {
			return fmt.Errorf("GetRepos(%s) failed: %v", orgName, err)
		}
		for _, r := range rs {
			repos = append(repos, r.Name)
		}
	} else {
		// Unopinionated org, just set explicitly defined repos
		for r := range org.Repos {
			repos = append(repos, r)
		}
	}

	for _, repoName := range repos {
		err := p.UpdateRepo(orgName, repoName, org.Repos[repoName])
		if err != nil {
			return err
		}
	}
	return nil
}

// Update all branches in the repo with the specified defaults
func (p *Protector) UpdateRepo(orgName string, repo string, repoDefaults config.Repo) error {
	p.completedRepos[orgName+"/"+repo] = true

	branches, err := p.client.GetBranches(orgName, repo)
	if err != nil {
		return fmt.Errorf("GetBranches(%s, %s) failed: %v", orgName, repo, err)
	}
	for _, branch := range branches {
		bn := branch.Name
		if err := p.UpdateBranch(orgName, repo, bn, branch.Protected); err != nil {
			return fmt.Errorf("UpdateBranch(%s, %s, %s, %t) failed: %v", orgName, repo, bn, branch.Protected, err)
		}
	}
	return nil
}

// Update the branch with the specified configuration
func (p *Protector) UpdateBranch(orgName, repo string, branchName string, protected bool) error {
	bp, err := p.cfg.GetBranchProtection(orgName, repo, branchName)
	if err != nil {
		return err
	}
	if bp == nil || bp.Protect == nil {
		return nil
	}
	if !protected && !*bp.Protect {
		logrus.Infof("%s/%s=%s: already unprotected", orgName, repo, branchName)
		return nil
	}
	var req *github.BranchProtectionRequest
	if *bp.Protect {
		r := makeRequest(*bp)
		req = &r
	}
	p.updates <- Requirements{
		Org:     orgName,
		Repo:    repo,
		Branch:  branchName,
		Request: req,
	}

	return nil
}
