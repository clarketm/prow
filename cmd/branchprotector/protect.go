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
	"log"
	"net/url"
	"strings"
	"sync"

	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/github"
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

func jobRequirements(jobs []config.Presubmit, after bool) []string {
	var required []string
	for _, j := range jobs {
		// Does this job require a context or have kids that might need one?
		if !after && !j.AlwaysRun && j.RunIfChanged == "" {
			continue // No
		}
		if !j.SkipReport { // This job needs a context
			required = append(required, j.Context)
		}
		// Check which children require contexts
		required = append(required, jobRequirements(j.RunAfterSuccess, true)...)
	}
	return required
}

func repoRequirements(org, repo string, cfg config.Config) []string {
	p, ok := cfg.Presubmits[org+"/"+repo]
	if !ok {
		return nil
	}
	return jobRequirements(p, false)
}

type Requirements struct {
	Org      string
	Repo     string
	Branch   string
	Protect  bool
	Contexts []string
	Pushers  []string
}

type Errors struct {
	lock sync.Mutex
	errs []error
}

func (e *Errors) add(err error) {
	e.lock.Lock()
	log.Println(err)
	defer e.lock.Unlock()
	e.errs = append(e.errs, err)
}

func main() {
	o := gatherOptions()
	if err := o.Validate(); err != nil {
		log.Fatal(err)
	}

	cfg, err := config.Load(o.config)
	if err != nil {
		log.Fatalf("Failed to load --config=%s: %v", o.config, err)
	}

	b, err := ioutil.ReadFile(o.token)
	if err != nil {
		log.Fatalf("cannot read --token: %v", err)
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
	if len(errors) > 0 {
		log.Fatalf("Encountered %d errors protecting branches: %v", len(errors), errors)
	}
}

type client interface {
	RemoveBranchProtection(org, repo, branch string) error
	UpdateBranchProtection(org, repo, branch string, contexts, pushers []string) error
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
	for r := range p.updates {
		if !r.Protect {
			err := p.client.RemoveBranchProtection(r.Org, r.Repo, r.Branch)
			if err != nil {
				p.errors.add(fmt.Errorf("remove %s/%s=%s protection failed: %v", r.Org, r.Repo, r.Branch, err))
			}
		} else {
			err := p.client.UpdateBranchProtection(r.Org, r.Repo, r.Branch, r.Contexts, r.Pushers)
			if err != nil {
				p.errors.add(fmt.Errorf("update %s/%s=%s protection failed: %v", r.Org, r.Repo, r.Branch, err))
			}
		}
	}
	p.done <- p.errors.errs
}

// Protect branches specified in the presubmit and branch-protection config sections.
func (p *Protector) Protect() {
	bp := p.cfg.BranchProtection

	// Scan the branch-protection configuration
	for orgName, org := range bp.Orgs {
		if err := p.UpdateOrg(orgName, org, bp.Protect, bp.Contexts, bp.Pushers); err != nil {
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
		if len(parts) != 2 {
			log.Fatalf("Bad repo: %s", repo)
		}
		orgName := parts[0]
		repoName := parts[1]
		repoReqs := repoRequirements(orgName, repoName, *p.cfg)
		protect := len(repoReqs) > 0
		if err := p.UpdateRepo(orgName, repoName, config.Repo{}, &protect, repoReqs, nil); err != nil {
			p.errors.add(err)
		}
	}
}

// Update all repos in the org with the specified defaults
func (p *Protector) UpdateOrg(orgName string, org config.Org, protect *bool, contexts, pushers []string) error {
	if org.Protect != nil {
		protect = org.Protect
	}

	oc := append([]string(nil), contexts...)
	oc = append(oc, org.Contexts...)

	op := append([]string(nil), pushers...)
	op = append(op, org.Pushers...)

	var repos []string
	if protect != nil {
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
		err := p.UpdateRepo(orgName, repoName, org.Repos[repoName], protect, oc, op)
		if err != nil {
			return err
		}
	}
	return nil
}

// Update all branches in the repo with the specified defaults
func (p *Protector) UpdateRepo(orgName string, repo string, repoDefaults config.Repo, protect *bool, contexts, pushers []string) error {
	p.completedRepos[orgName+"/"+repo] = true

	rc := append([]string(nil), contexts...)
	rp := append([]string(nil), pushers...)

	if repoDefaults.Protect != nil {
		protect = repoDefaults.Protect
	}
	rc = append(rc, repoDefaults.Contexts...)
	rp = append(rp, repoDefaults.Pushers...)

	rc = append(rc, repoRequirements(orgName, repo, *p.cfg)...)

	var branches []string
	if protect != nil {
		bs, err := p.client.GetBranches(orgName, repo)
		if err != nil {
			return fmt.Errorf("GetBranches(%s, %s) failed: %v", orgName, repo, err)
		}
		for _, branch := range bs {
			branches = append(branches, branch.Name)
		}
	} else {
		for b := range repoDefaults.Branches {
			branches = append(branches, b)
		}
	}

	for _, branchName := range branches {
		if err := p.UpdateBranch(orgName, repo, branchName, repoDefaults.Branches[branchName], protect, rc, rp); err != nil {
			return fmt.Errorf("UpdateBranch(%s, %s, %s, ...) failed: %v", orgName, repo, branchName, err)
		}
	}
	return nil
}

// Update the branch with the specified configuration
func (p *Protector) UpdateBranch(orgName, repo string, branchName string, branchDefaults config.Branch, protect *bool, contexts, pushers []string) error {
	bc := append([]string{}, contexts...)
	bpush := append([]string{}, pushers...)
	if branchDefaults.Protect != nil {
		protect = branchDefaults.Protect
	}
	bc = append(bc, branchDefaults.Contexts...)
	bpush = append(bpush, branchDefaults.Pushers...)

	if protect == nil {
		return errors.New("protect should not be nil")
	}

	prot := *protect

	if len(bc) > 0 || len(bpush) > 0 {
		if !prot {
			return errors.New("setting pushers or contexts requires protection")
		}
	}

	p.updates <- Requirements{
		Org:      orgName,
		Repo:     repo,
		Branch:   branchName,
		Protect:  prot,
		Contexts: bc,
		Pushers:  bpush,
	}
	return nil
}
