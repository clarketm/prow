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

package trigger

import (
	"fmt"

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/plugins"
)

func handlePR(c client, pr github.PullRequestEvent) error {
	switch pr.Action {
	case "opened":
		// When a PR is opened, if the author is in the org then build it.
		// Otherwise, ask for "ok to test". There's no need to look for previous
		// "ok to test" comments since the PR was just opened!
		member, err := c.GitHubClient.IsMember(trustedOrg, pr.PullRequest.User.Login)
		if err != nil {
			return fmt.Errorf("could not check membership: %s", err)
		} else if member {
			return buildAll(c, pr.PullRequest)
		} else if err := askToJoin(c.GitHubClient, pr.PullRequest); err != nil {
			return fmt.Errorf("could not ask to join: %s", err)
		}
	case "reopened", "synchronize":
		// When a PR is updated, check that the user is in the org or that an org
		// member has said "ok to test" before building. There's no need to ask
		// for "ok to test" because we do that once when the PR is created.
		trusted, err := trustedPullRequest(c.GitHubClient, pr.PullRequest)
		if err != nil {
			return fmt.Errorf("could not validate PR: %s", err)
		} else if trusted {
			return buildAll(c, pr.PullRequest)
		}
	case "closed":
		return deleteAll(c, pr.PullRequest)
	case "labeled":
		// When a PR is LGTMd, if it is untrusted then build it once.
		if pr.Label.Name == lgtmLabel {
			trusted, err := trustedPullRequest(c.GitHubClient, pr.PullRequest)
			if err != nil {
				return fmt.Errorf("could not validate PR: %s", err)
			} else if !trusted {
				return buildAll(c, pr.PullRequest)
			}
		}
	}
	return nil
}

func askToJoin(ghc githubClient, pr github.PullRequest) error {
	commentTemplate := `
Can a [%s](https://github.com/orgs/%s/people) member verify that this patch is reasonable to test? If so, please reply with "@k8s-bot ok to test" on its own line. Until that is done, I will not automatically test new commits in this PR, but the usual testing commands will still work. Regular contributors should join the org to skip this step.

<details>
%s
</details
`
	comment := fmt.Sprintf(commentTemplate, trustedOrg, trustedOrg, plugins.AboutThisBot)

	owner := pr.Base.Repo.Owner.Login
	name := pr.Base.Repo.Name
	return ghc.CreateComment(owner, name, pr.Number, comment)
}

// trustedPullRequest returns whether or not the given PR should be tested.
// It first checks if the author is in the org, then looks for "ok to test
// comments by org members.
func trustedPullRequest(ghc githubClient, pr github.PullRequest) (bool, error) {
	author := pr.User.Login
	// First check if the author is a member of the org.
	orgMember, err := ghc.IsMember(trustedOrg, author)
	if err != nil {
		return false, err
	} else if orgMember {
		return true, nil
	}
	// Next look for "ok to test" comments on the PR.
	comments, err := ghc.ListIssueComments(pr.Base.Repo.Owner.Login, pr.Base.Repo.Name, pr.Number)
	if err != nil {
		return false, err
	}
	for _, comment := range comments {
		commentAuthor := comment.User.Login
		// Skip comments by the PR author.
		if commentAuthor == author {
			continue
		}
		// Skip bot comments.
		if commentAuthor == "k8s-bot" || commentAuthor == "k8s-ci-robot" {
			continue
		}
		// Look for "ok to test"
		if !okToTest.MatchString(comment.Body) {
			continue
		}
		// Ensure that the commenter is in the org.
		commentAuthorMember, err := ghc.IsMember(trustedOrg, commentAuthor)
		if err != nil {
			return false, err
		} else if commentAuthorMember {
			return true, nil
		}
	}
	return false, nil
}
