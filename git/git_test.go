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

package git_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/clarketm/prow/git/localgit"
	"github.com/clarketm/prow/github"
)

func TestClone(t *testing.T) {
	lg, c, err := localgit.New()
	if err != nil {
		t.Fatalf("Making local git repo: %v", err)
	}
	defer func() {
		if err := lg.Clean(); err != nil {
			t.Errorf("Error cleaning LocalGit: %v", err)
		}
		if err := c.Clean(); err != nil {
			t.Errorf("Error cleaning Client: %v", err)
		}
	}()
	if err := lg.MakeFakeRepo("foo", "bar"); err != nil {
		t.Fatalf("Making fake repo: %v", err)
	}
	if err := lg.MakeFakeRepo("foo", "baz"); err != nil {
		t.Fatalf("Making fake repo: %v", err)
	}

	// Fresh clone, will be a cache miss.
	r1, err := c.Clone("foo/bar")
	if err != nil {
		t.Fatalf("Cloning the first time: %v", err)
	}
	defer func() {
		if err := r1.Clean(); err != nil {
			t.Errorf("Cleaning repo: %v", err)
		}
	}()

	// Clone from the same org.
	r2, err := c.Clone("foo/baz")
	if err != nil {
		t.Fatalf("Cloning another repo in the same org: %v", err)
	}
	defer func() {
		if err := r2.Clean(); err != nil {
			t.Errorf("Cleaning repo: %v", err)
		}
	}()

	// Make sure it fetches when we clone again.
	if err := lg.AddCommit("foo", "bar", map[string][]byte{"second": {}}); err != nil {
		t.Fatalf("Adding second commit: %v", err)
	}
	r3, err := c.Clone("foo/bar")
	if err != nil {
		t.Fatalf("Cloning a second time: %v", err)
	}
	defer func() {
		if err := r3.Clean(); err != nil {
			t.Errorf("Cleaning repo: %v", err)
		}
	}()
	log := exec.Command("git", "log", "--oneline")
	log.Dir = r3.Directory()
	if b, err := log.CombinedOutput(); err != nil {
		t.Fatalf("git log: %v, %s", err, string(b))
	} else {
		t.Logf("git log output: %s", string(b))
		if len(bytes.Split(bytes.TrimSpace(b), []byte("\n"))) != 2 {
			t.Error("Wrong number of commits in git log output. Expected 2")
		}
	}
}

func TestCheckoutPR(t *testing.T) {
	lg, c, err := localgit.New()
	if err != nil {
		t.Fatalf("Making local git repo: %v", err)
	}
	defer func() {
		if err := lg.Clean(); err != nil {
			t.Errorf("Error cleaning LocalGit: %v", err)
		}
		if err := c.Clean(); err != nil {
			t.Errorf("Error cleaning Client: %v", err)
		}
	}()
	if err := lg.MakeFakeRepo("foo", "bar"); err != nil {
		t.Fatalf("Making fake repo: %v", err)
	}
	r, err := c.Clone("foo/bar")
	if err != nil {
		t.Fatalf("Cloning: %v", err)
	}
	defer func() {
		if err := r.Clean(); err != nil {
			t.Errorf("Cleaning repo: %v", err)
		}
	}()

	if err := lg.CheckoutNewBranch("foo", "bar", "pull/123/head"); err != nil {
		t.Fatalf("Checkout new branch: %v", err)
	}
	if err := lg.AddCommit("foo", "bar", map[string][]byte{"wow": {}}); err != nil {
		t.Fatalf("Add commit: %v", err)
	}

	if err := r.CheckoutPullRequest(123); err != nil {
		t.Fatalf("Checking out PR: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.Directory(), "wow")); err != nil {
		t.Errorf("Didn't find file in PR after checking out: %v", err)
	}
}

func TestMergeCommitsExistBetween(t *testing.T) {
	lg, c, err := localgit.New()
	if err != nil {
		t.Fatalf("Making localgit: %v", err)
	}
	defer func() {
		if err := lg.Clean(); err != nil {
			t.Errorf("Cleaning up localgit: %v", err)
		}
		if err := c.Clean(); err != nil {
			t.Errorf("Cleaning up client: %v", err)
		}
	}()
	if err := lg.MakeFakeRepo("foo", "bar"); err != nil {
		t.Fatalf("Making fake repo: %v", err)
	}
	r, err := c.Clone("foo/bar")
	if err != nil {
		t.Fatalf("Cloning: %v", err)
	}
	defer func() {
		if err := r.Clean(); err != nil {
			t.Errorf("Cleaning repo: %v", err)
		}
	}()
	var (
		checkoutPR = func(prNum int) {
			if err := lg.CheckoutNewBranch("foo", "bar", fmt.Sprintf("pull/%d/head", prNum)); err != nil {
				t.Fatalf("Creating & checking out pull branch pull/%d/head: %v", prNum, err)
			}
		}
		checkoutBranch = func(branch string) {
			if err := lg.Checkout("foo", "bar", branch); err != nil {
				t.Fatalf("Checking out branch %s: %v", branch, err)
			}
		}
		addCommit = func(file string) {
			if err := lg.AddCommit("foo", "bar", map[string][]byte{file: {}}); err != nil {
				t.Fatalf("Adding commit: %v", err)
			}
		}
		mergeMaster = func() {
			if _, err := lg.Merge("foo", "bar", "master"); err != nil {
				t.Fatalf("Rebasing commit: %v", err)
			}
		}
		rebaseMaster = func() {
			if _, err := lg.Rebase("foo", "bar", "master"); err != nil {
				t.Fatalf("Rebasing commit: %v", err)
			}
		}
	)

	type testCase struct {
		name          string
		prNum         int
		checkout      func()
		mergeOrRebase func()
		checkoutPR    func() error
		want          bool
	}
	testcases := []testCase{
		{
			name:          "PR has merge commits",
			prNum:         1,
			checkout:      func() { checkoutBranch("pull/1/head") },
			mergeOrRebase: mergeMaster,
			checkoutPR:    func() error { return r.CheckoutPullRequest(1) },
			want:          true,
		},
		{
			name:          "PR doesn't have merge commits",
			prNum:         2,
			checkout:      func() { checkoutBranch("pull/2/head") },
			mergeOrRebase: rebaseMaster,
			checkoutPR:    func() error { return r.CheckoutPullRequest(2) },
			want:          false,
		},
	}

	addCommit("wow")
	// preparation work: branch off all prs upon commit 'wow'
	for _, tt := range testcases {
		checkoutPR(tt.prNum)
	}
	// switch back to master and create a new commit 'ouch'
	checkoutBranch("master")
	addCommit("ouch")
	masterSHA, err := lg.RevParse("foo", "bar", "HEAD")
	if err != nil {
		t.Fatalf("Fetching SHA: %v", err)
	}

	for _, tt := range testcases {
		tt.checkout()
		tt.mergeOrRebase()
		prSHA, err := lg.RevParse("foo", "bar", "HEAD")
		if err != nil {
			t.Fatalf("Fetching SHA: %v", err)
		}
		if err := tt.checkoutPR(); err != nil {
			t.Fatalf("Checking out PR: %v", err)
		}
		// verify the content is up to dated
		ouchPath := filepath.Join(r.Directory(), "ouch")
		if _, err := os.Stat(ouchPath); err != nil {
			t.Fatalf("Didn't find file 'ouch' in PR %d after merging: %v", tt.prNum, err)
		}

		got, err := r.MergeCommitsExistBetween(masterSHA, prSHA)
		key := fmt.Sprintf("foo/bar/%d", tt.prNum)
		if err != nil {
			t.Errorf("Case: %v. Expect err is nil, but got %v", key, err)
		}
		if tt.want != got {
			t.Errorf("Case: %v. Expect MergeCommitsExistBetween()=%v, but got %v", key, tt.want, got)
		}
	}
}

func TestMergeAndCheckout(t *testing.T) {
	testCases := []struct {
		name          string
		setBaseSHA    bool
		prBranches    []string
		mergeStrategy github.PullRequestMergeType
		err           string
	}{
		{
			name: "Unset baseSHA, error",
			err:  "baseSHA must be set",
		},
		{
			name:       "No mergeStrategy, error",
			setBaseSHA: true,
			prBranches: []string{"my-pr-branch"},
			err:        "merge strategy \"\" is not supported",
		},
		{
			name:          "Merge strategy rebase, error",
			setBaseSHA:    true,
			prBranches:    []string{"my-pr-branch"},
			mergeStrategy: github.MergeRebase,
			err:           "merge strategy \"rebase\" is not supported",
		},
		{
			name:       "No pullRequestHead, error",
			setBaseSHA: true,
		},
		{
			name:          "Merge succeeds with one head and merge strategy",
			setBaseSHA:    true,
			prBranches:    []string{"my-pr-branch"},
			mergeStrategy: github.MergeMerge,
		},
		{
			name:          "Merge succeeds with multiple heads and merge strategy",
			setBaseSHA:    true,
			prBranches:    []string{"my-pr-branch", "my-other-pr-branch"},
			mergeStrategy: github.MergeMerge,
		},
		{
			name:          "Merge succeeds with one head and squash strategy",
			setBaseSHA:    true,
			prBranches:    []string{"my-pr-branch"},
			mergeStrategy: github.MergeSquash,
		},
		{
			name:          "Merge succeeds with multiple heads and squash stragey",
			setBaseSHA:    true,
			prBranches:    []string{"my-pr-branch", "my-other-pr-branch"},
			mergeStrategy: github.MergeSquash,
		},
	}

	const (
		org  = "my-org"
		repo = "my-repo"
	)
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc := tc
			t.Parallel()

			lg, c, err := localgit.New()
			if err != nil {
				t.Fatalf("Making local git repo: %v", err)
			}
			logrus.SetLevel(logrus.DebugLevel)
			defer func() {
				if err := lg.Clean(); err != nil {
					t.Errorf("Error cleaning LocalGit: %v", err)
				}
				if err := c.Clean(); err != nil {
					t.Errorf("Error cleaning Client: %v", err)
				}
			}()
			if err := lg.MakeFakeRepo(org, repo); err != nil {
				t.Fatalf("Making fake repo: %v", err)
			}

			var commitsToMerge []string
			for _, prBranch := range tc.prBranches {
				if err := lg.CheckoutNewBranch(org, repo, prBranch); err != nil {
					t.Fatalf("failed to checkout new branch %q: %v", prBranch, err)
				}
				if err := lg.AddCommit(org, repo, map[string][]byte{prBranch: []byte("val")}); err != nil {
					t.Fatalf("failed to add commit: %v", err)
				}
				headRef, err := lg.RevParse(org, repo, "HEAD")
				if err != nil {
					t.Fatalf("failed to run git rev-parse: %v", err)
				}
				commitsToMerge = append(commitsToMerge, headRef)
			}
			if len(tc.prBranches) > 0 {
				if err := lg.Checkout(org, repo, "master"); err != nil {
					t.Fatalf("failed to run git checkout master: %v", err)
				}
			}

			var baseSHA string
			if tc.setBaseSHA {
				baseSHA, err = lg.RevParse(org, repo, "master")
				if err != nil {
					t.Fatalf("failed to run git rev-parse master: %v", err)
				}
			}

			clonedRepo, err := c.Clone(org + "/" + repo)
			if err != nil {
				t.Fatalf("Cloning failed: %v", err)
			}
			if err := clonedRepo.Config("user.name", "prow"); err != nil {
				t.Fatalf("failed to set name for test repo: %v", err)
			}
			if err := clonedRepo.Config("user.email", "prow@localhost"); err != nil {
				t.Fatalf("failed to set email for test repo: %v", err)
			}
			if err := clonedRepo.Config("commit.gpgsign", "false"); err != nil {
				t.Fatalf("failed to disable gpg signing for test repo: %v", err)
			}

			err = clonedRepo.MergeAndCheckout(baseSHA, tc.mergeStrategy, commitsToMerge...)
			if err == nil && tc.err == "" {
				return
			}
			if err == nil || err.Error() != tc.err {
				t.Errorf("Expected err %q but got \"%v\"", tc.err, err)
			}
		})
	}

}
