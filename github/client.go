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

package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shurcooL/githubql"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
)

type timeClient interface {
	Sleep(time.Duration)
	Until(time.Time) time.Duration
}

type standardTime struct{}

func (s *standardTime) Sleep(d time.Duration) {
	time.Sleep(d)
}
func (s *standardTime) Until(t time.Time) time.Duration {
	return time.Until(t)
}

type Client struct {
	// If logger is non-nil, log all method calls with it.
	logger *logrus.Entry
	time   timeClient

	gqlc     gqlClient
	client   httpClient
	token    string
	base     string
	dry      bool
	fake     bool
	throttle throttler

	mut     sync.Mutex // protects botName and email
	botName string
	email   string
}

const (
	maxRetries    = 8
	max404Retries = 2
	maxSleepTime  = 2 * time.Minute
	initialDelay  = 2 * time.Second
)

// Interface for how prow interacts with the http client, which we may throttle.
type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Interface for how prow interacts with the graphql client, which we may throttle.
type gqlClient interface {
	Query(ctx context.Context, q interface{}, vars map[string]interface{}) error
}

// throttler sets a ceiling on the rate of github requests.
// Configure with Client.Throttle()
type throttler struct {
	ticker   *time.Ticker
	throttle chan time.Time
	http     httpClient
	graph    gqlClient
	slow     int32 // Helps log once when requests start/stop being throttled
	lock     sync.RWMutex
}

func (t *throttler) Wait() {
	log := logrus.WithFields(logrus.Fields{"client": "github", "throttled": true})
	t.lock.RLock()
	defer t.lock.RUnlock()
	var more bool
	select {
	case _, more = <-t.throttle:
		// If we were throttled and the channel is now somewhat (25%+) full, note this
		if len(t.throttle) > cap(t.throttle)/4 && atomic.CompareAndSwapInt32(&t.slow, 1, 0) {
			log.Debug("Unthrottled")
		}
		if !more {
			log.Debug("Throttle channel closed")
		}
		return
	default: // Do not wait if nothing is available right now
	}
	// If this is the first time we are waiting, note this
	if slow := atomic.SwapInt32(&t.slow, 1); slow == 0 {
		log.Debug("Throttled")
	}
	_, more = <-t.throttle
	if !more {
		log.Debug("Throttle channel closed")
	}
}

func (t *throttler) Do(req *http.Request) (*http.Response, error) {
	t.Wait()
	return t.http.Do(req)
}

func (t *throttler) Query(ctx context.Context, q interface{}, vars map[string]interface{}) error {
	t.Wait()
	return t.graph.Query(ctx, q, vars)
}

// Throttle client to a rate of at most hourlyTokens requests per hour,
// allowing burst tokens.
func (c *Client) Throttle(hourlyTokens, burst int) {
	c.log("Throttle", hourlyTokens, burst)
	c.throttle.lock.Lock()
	defer c.throttle.lock.Unlock()
	previouslyThrottled := c.throttle.ticker != nil
	if hourlyTokens <= 0 || burst <= 0 { // Disable throttle
		if previouslyThrottled { // Unwrap clients if necessary
			c.client = c.throttle.http
			c.gqlc = c.throttle.graph
			c.throttle.ticker.Stop()
			c.throttle.ticker = nil
		}
		return
	}
	rate := time.Hour / time.Duration(hourlyTokens)
	ticker := time.NewTicker(rate)
	throttle := make(chan time.Time, burst)
	for i := 0; i < burst; i++ { // Fill up the channel
		throttle <- time.Now()
	}
	go func() {
		// Refill the channel
		for t := range ticker.C {
			select {
			case throttle <- t:
			default:
			}
		}
	}()
	if !previouslyThrottled { // Wrap clients if we haven't already
		c.throttle.http = c.client
		c.throttle.graph = c.gqlc
		c.client = &c.throttle
		c.gqlc = &c.throttle
	}
	c.throttle.ticker = ticker
	c.throttle.throttle = throttle
}

// NewClient creates a new fully operational GitHub client.
func NewClient(token, base string) *Client {
	return &Client{
		logger: logrus.WithField("client", "github"),
		time:   &standardTime{},
		gqlc:   githubql.NewClient(oauth2.NewClient(context.Background(), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}))),
		client: &http.Client{},
		token:  token,
		base:   base,
		dry:    false,
	}
}

// NewDryRunClient creates a new client that will not perform mutating actions
// such as setting statuses or commenting, but it will still query GitHub and
// use up API tokens.
func NewDryRunClient(token, base string) *Client {
	return &Client{
		logger: logrus.WithField("client", "github"),
		time:   &standardTime{},
		gqlc:   githubql.NewClient(oauth2.NewClient(context.Background(), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}))),
		client: &http.Client{},
		token:  token,
		base:   base,
		dry:    true,
	}
}

// NewFakeClient creates a new client that will not perform any actions at all.
func NewFakeClient() *Client {
	return &Client{
		logger: logrus.WithField("client", "github"),
		time:   &standardTime{},
		fake:   true,
		dry:    true,
	}
}

func (c *Client) log(methodName string, args ...interface{}) {
	if c.logger == nil {
		return
	}
	var as []string
	for _, arg := range args {
		as = append(as, fmt.Sprintf("%v", arg))
	}
	c.logger.Infof("%s(%s)", methodName, strings.Join(as, ", "))
}

type request struct {
	method      string
	path        string
	accept      string
	requestBody interface{}
	exitCodes   []int
}

type requestError struct {
	ClientError
	ErrorString string
}

func (r requestError) Error() string {
	return r.ErrorString
}

// Make a request with retries. If ret is not nil, unmarshal the response body
// into it. Returns an error if the exit code is not one of the provided codes.
func (c *Client) request(r *request, ret interface{}) (int, error) {
	statusCode, b, err := c.requestRaw(r)
	if err != nil {
		return statusCode, err
	}
	if ret != nil {
		if err := json.Unmarshal(b, ret); err != nil {
			return statusCode, err
		}
	}
	return statusCode, nil
}

// requestRaw makes a request with retries and returns the response body.
// Returns an error if the exit code is not one of the provided codes.
func (c *Client) requestRaw(r *request) (int, []byte, error) {
	if c.fake || (c.dry && r.method != http.MethodGet) {
		return r.exitCodes[0], nil, nil
	}
	resp, err := c.requestRetry(r.method, r.path, r.accept, r.requestBody)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	var okCode bool
	for _, code := range r.exitCodes {
		if code == resp.StatusCode {
			okCode = true
			break
		}
	}
	if !okCode {
		clientError := ClientError{}
		if err := json.Unmarshal(b, &clientError); err != nil {
			return resp.StatusCode, b, err
		}
		return resp.StatusCode, b, requestError{
			ClientError: clientError,
			ErrorString: fmt.Sprintf("status code %d not one of %v, body: %s", resp.StatusCode, r.exitCodes, string(b)),
		}
	}
	return resp.StatusCode, b, nil
}

// Retry on transport failures. Retries on 500s, retries after sleep on
// ratelimit exceeded, and retries 404s a couple times.
// This function closes the response body iff it also returns an error.
func (c *Client) requestRetry(method, path, accept string, body interface{}) (*http.Response, error) {
	var resp *http.Response
	var err error
	backoff := initialDelay
	for retries := 0; retries < maxRetries; retries++ {
		if retries > 0 && resp != nil {
			resp.Body.Close()
		}
		resp, err = c.doRequest(method, path, accept, body)
		if err == nil {
			if resp.StatusCode == 404 && retries < max404Retries {
				// Retry 404s a couple times. Sometimes GitHub is inconsistent in
				// the sense that they send us an event such as "PR opened" but an
				// immediate request to GET the PR returns 404. We don't want to
				// retry more than a couple times in this case, because a 404 may
				// be caused by a bad API call and we'll just burn through API
				// tokens.
				c.time.Sleep(backoff)
				backoff *= 2
			} else if resp.StatusCode == 403 {
				if resp.Header.Get("X-RateLimit-Remaining") == "0" {
					// If we are out of API tokens, sleep first. The X-RateLimit-Reset
					// header tells us the time at which we can request again.
					var t int
					if t, err = strconv.Atoi(resp.Header.Get("X-RateLimit-Reset")); err == nil {
						// Sleep an extra second plus how long GitHub wants us to
						// sleep. If it's going to take too long, then break.
						sleepTime := c.time.Until(time.Unix(int64(t), 0)) + time.Second
						if sleepTime < maxSleepTime {
							c.time.Sleep(sleepTime)
						} else {
							err = fmt.Errorf("sleep time for token reset exceeds max sleep time (%v > %v)", sleepTime, maxSleepTime)
							resp.Body.Close()
							break
						}
					} else {
						err = fmt.Errorf("failed to parse rate limit reset unix time %q: %v", resp.Header.Get("X-RateLimit-Reset"), err)
						resp.Body.Close()
						break
					}
				} else if oauthScopes := resp.Header.Get("X-Accepted-OAuth-Scopes"); len(oauthScopes) > 0 {
					err = fmt.Errorf("is the account using at least one of the following oauth scopes?: %s", oauthScopes)
					resp.Body.Close()
					break
				}
			} else if resp.StatusCode < 500 {
				// Normal, happy case.
				break
			} else {
				// Retry 500 after a break.
				c.time.Sleep(backoff)
				backoff *= 2
			}
		} else {
			c.time.Sleep(backoff)
			backoff *= 2
		}
	}
	return resp, err
}

func (c *Client) doRequest(method, path, accept string, body interface{}) (*http.Response, error) {
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		buf = bytes.NewBuffer(b)
	}
	req, err := http.NewRequest(method, path, buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+c.token)
	if accept == "" {
		req.Header.Add("Accept", "application/vnd.github.v3+json")
	} else {
		req.Header.Add("Accept", accept)
	}
	// Disable keep-alive so that we don't get flakes when GitHub closes the
	// connection prematurely.
	// https://go-review.googlesource.com/#/c/3210/ fixed it for GET, but not
	// for POST.
	req.Close = true
	return c.client.Do(req)
}

// Not thread-safe - callers need to hold c.mut.
func (c *Client) getUserData() error {
	var u User
	_, err := c.request(&request{
		method:    http.MethodGet,
		path:      fmt.Sprintf("%s/user", c.base),
		exitCodes: []int{200},
	}, &u)
	if err != nil {
		return err
	}
	c.botName = u.Login
	// email needs to be publicly accessible via the profile
	// of the current account. Read below for more info
	// https://developer.github.com/v3/users/#get-a-single-user
	c.email = u.Email
	return nil
}

func (c *Client) BotName() (string, error) {
	c.mut.Lock()
	defer c.mut.Unlock()
	if c.botName == "" {
		if err := c.getUserData(); err != nil {
			return "", fmt.Errorf("fetching bot name from GitHub: %v", err)
		}
	}
	return c.botName, nil
}

func (c *Client) Email() (string, error) {
	c.mut.Lock()
	defer c.mut.Unlock()
	if c.email == "" {
		if err := c.getUserData(); err != nil {
			return "", fmt.Errorf("fetching e-mail from GitHub: %v", err)
		}
	}
	return c.email, nil
}

// IsMember returns whether or not the user is a member of the org.
func (c *Client) IsMember(org, user string) (bool, error) {
	c.log("IsMember", org, user)
	if org == user {
		// Make it possible to run a couple of plugins on personal repos.
		return true, nil
	}
	code, err := c.request(&request{
		method:    http.MethodGet,
		path:      fmt.Sprintf("%s/orgs/%s/members/%s", c.base, org, user),
		exitCodes: []int{204, 404, 302},
	}, nil)
	if err != nil {
		return false, err
	}
	if code == 204 {
		return true, nil
	} else if code == 404 {
		return false, nil
	} else if code == 302 {
		return false, fmt.Errorf("requester is not %s org member", org)
	}
	// Should be unreachable.
	return false, fmt.Errorf("unexpected status: %d", code)
}

// ListOrgMembers list all users who are members of an organization. If the authenticated
// user is also a member of this organization then both concealed and public members
// will be returned.
//
// https://developer.github.com/v3/orgs/members/#members-list
func (c *Client) ListOrgMembers(org string) ([]TeamMember, error) {
	c.log("ListOrgMembers", org)
	if c.fake {
		return nil, nil
	}
	path := fmt.Sprintf("/orgs/%s/members", org)
	var teamMembers []TeamMember
	err := c.readPaginatedResults(
		path,
		"",
		func() interface{} {
			return &[]TeamMember{}
		},
		func(obj interface{}) {
			teamMembers = append(teamMembers, *(obj.(*[]TeamMember))...)
		},
	)
	if err != nil {
		return nil, err
	}
	return teamMembers, nil
}

// CreateComment creates a comment on the issue.
func (c *Client) CreateComment(org, repo string, number int, comment string) error {
	c.log("CreateComment", org, repo, number, comment)
	ic := IssueComment{
		Body: comment,
	}
	_, err := c.request(&request{
		method:      http.MethodPost,
		path:        fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", c.base, org, repo, number),
		requestBody: &ic,
		exitCodes:   []int{201},
	}, nil)
	return err
}

// DeleteComment deletes the comment.
func (c *Client) DeleteComment(org, repo string, ID int) error {
	c.log("DeleteComment", org, repo, ID)
	_, err := c.request(&request{
		method:    http.MethodDelete,
		path:      fmt.Sprintf("%s/repos/%s/%s/issues/comments/%d", c.base, org, repo, ID),
		exitCodes: []int{204},
	}, nil)
	return err
}

func (c *Client) EditComment(org, repo string, ID int, comment string) error {
	c.log("EditComment", org, repo, ID, comment)
	ic := IssueComment{
		Body: comment,
	}
	_, err := c.request(&request{
		method:      http.MethodPatch,
		path:        fmt.Sprintf("%s/repos/%s/%s/issues/comments/%d", c.base, org, repo, ID),
		requestBody: &ic,
		exitCodes:   []int{200},
	}, nil)
	return err
}

func (c *Client) CreateCommentReaction(org, repo string, ID int, reaction string) error {
	c.log("CreateCommentReaction", org, repo, ID, reaction)
	r := Reaction{Content: reaction}
	_, err := c.request(&request{
		method:      http.MethodPost,
		path:        fmt.Sprintf("%s/repos/%s/%s/issues/comments/%d/reactions", c.base, org, repo, ID),
		accept:      "application/vnd.github.squirrel-girl-preview",
		exitCodes:   []int{201},
		requestBody: &r,
	}, nil)
	return err
}

func (c *Client) CreateIssueReaction(org, repo string, ID int, reaction string) error {
	c.log("CreateIssueReaction", org, repo, ID, reaction)
	r := Reaction{Content: reaction}
	_, err := c.request(&request{
		method:      http.MethodPost,
		path:        fmt.Sprintf("%s/repos/%s/%s/issues/%d/reactions", c.base, org, repo, ID),
		accept:      "application/vnd.github.squirrel-girl-preview",
		requestBody: &r,
		exitCodes:   []int{200, 201},
	}, nil)
	return err
}

// DeleteStaleComments iterates over comments on an issue/PR, deleting those which the 'isStale'
// function identifies as stale. If 'comments' is nil, the comments will be fetched from github.
func (c *Client) DeleteStaleComments(org, repo string, number int, comments []IssueComment, isStale func(IssueComment) bool) error {
	var err error
	if comments == nil {
		comments, err = c.ListIssueComments(org, repo, number)
		if err != nil {
			return fmt.Errorf("failed to list comments while deleting stale comments. err: %v", err)
		}
	}
	for _, comment := range comments {
		if isStale(comment) {
			if err := c.DeleteComment(org, repo, comment.ID); err != nil {
				return fmt.Errorf("failed to delete stale comment with ID '%d'", comment.ID)
			}
		}
	}
	return nil
}

// readPaginatedResults iterates over all objects in the paginated
// result indicated by the given url.  The newObj function should
// return a new slice of the expected type, and the accumulate
// function should accept that populated slice for each page of
// results.  An error is returned if encountered in making calls to
// github or marshalling objects.
func (c *Client) readPaginatedResults(path, accept string, newObj func() interface{}, accumulate func(interface{})) error {
	url := fmt.Sprintf("%s%s?per_page=100", c.base, path)
	for url != "" {
		resp, err := c.requestRetry(http.MethodGet, url, accept, nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return fmt.Errorf("return code not 2XX: %s", resp.Status)
		}

		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		obj := newObj()
		if err := json.Unmarshal(b, obj); err != nil {
			return err
		}

		accumulate(obj)

		url = parseLinks(resp.Header.Get("Link"))["next"]
	}
	return nil
}

// ListIssueComments returns all comments on an issue. This may use more than
// one API token.
func (c *Client) ListIssueComments(org, repo string, number int) ([]IssueComment, error) {
	c.log("ListIssueComments", org, repo, number)
	if c.fake {
		return nil, nil
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", org, repo, number)
	var comments []IssueComment
	err := c.readPaginatedResults(
		path,
		"",
		func() interface{} {
			return &[]IssueComment{}
		},
		func(obj interface{}) {
			comments = append(comments, *(obj.(*[]IssueComment))...)
		},
	)
	if err != nil {
		return nil, err
	}
	return comments, nil
}

// GetPullRequest gets a pull request.
func (c *Client) GetPullRequest(org, repo string, number int) (*PullRequest, error) {
	c.log("GetPullRequest", org, repo, number)
	var pr PullRequest
	_, err := c.request(&request{
		method:    http.MethodGet,
		path:      fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.base, org, repo, number),
		exitCodes: []int{200},
	}, &pr)
	return &pr, err
}

// GetPullRequestPatch gets the patch version of a pull request.
func (c *Client) GetPullRequestPatch(org, repo string, number int) ([]byte, error) {
	c.log("GetPullRequestPatch", org, repo, number)
	_, patch, err := c.requestRaw(&request{
		accept:    "application/vnd.github.VERSION.patch",
		method:    http.MethodGet,
		path:      fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.base, org, repo, number),
		exitCodes: []int{200},
	})
	return patch, err
}

// CreatePullRequest creates a new pull request and returns its number if
// the creation is successful, otherwise any error that is encountered.
func (c *Client) CreatePullRequest(org, repo, title, body, head, base string, canModify bool) (int, error) {
	c.log("CreatePullRequest", org, repo, title)
	data := struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		// MaintainerCanModify allows maintainers of the repo to modify this
		// pull request, eg. push changes to it before merging.
		MaintainerCanModify bool `json:"maintainer_can_modify"`
	}{
		Title: title,
		Body:  body,
		Head:  head,
		Base:  base,

		MaintainerCanModify: canModify,
	}
	var resp struct {
		Num int `json:"number"`
	}
	_, err := c.request(&request{
		method:      http.MethodPost,
		path:        fmt.Sprintf("%s/repos/%s/%s/pulls", c.base, org, repo),
		requestBody: &data,
		exitCodes:   []int{201},
	}, &resp)
	if err != nil {
		return 0, err
	}
	return resp.Num, nil
}

// GetPullRequestChanges gets a list of files modified in a pull request.
func (c *Client) GetPullRequestChanges(org, repo string, number int) ([]PullRequestChange, error) {
	c.log("GetPullRequestChanges", org, repo, number)
	if c.fake {
		return []PullRequestChange{}, nil
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/files", org, repo, number)
	var changes []PullRequestChange
	err := c.readPaginatedResults(
		path,
		"",
		func() interface{} {
			return &[]PullRequestChange{}
		},
		func(obj interface{}) {
			changes = append(changes, *(obj.(*[]PullRequestChange))...)
		},
	)
	if err != nil {
		return nil, err
	}
	return changes, nil
}

// ListPullRequestComments returns all *review* comments on a pull request. This may use
// more than one API token.
func (c *Client) ListPullRequestComments(org, repo string, number int) ([]ReviewComment, error) {
	c.log("ListPullRequestComments", org, repo, number)
	if c.fake {
		return nil, nil
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/comments", org, repo, number)
	var comments []ReviewComment
	err := c.readPaginatedResults(
		path,
		"",
		func() interface{} {
			return &[]ReviewComment{}
		},
		func(obj interface{}) {
			comments = append(comments, *(obj.(*[]ReviewComment))...)
		},
	)
	if err != nil {
		return nil, err
	}
	return comments, nil
}

// ListReviews returns all reviews on a pull request. This may use
// more than one API token.
func (c *Client) ListReviews(org, repo string, number int) ([]Review, error) {
	c.log("ListReviews", org, repo, number)
	if c.fake {
		return nil, nil
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", org, repo, number)
	var reviews []Review
	err := c.readPaginatedResults(
		path,
		"",
		func() interface{} {
			return &[]Review{}
		},
		func(obj interface{}) {
			reviews = append(reviews, *(obj.(*[]Review))...)
		},
	)
	if err != nil {
		return nil, err
	}
	return reviews, nil
}

// CreateStatus creates or updates the status of a commit.
func (c *Client) CreateStatus(org, repo, sha string, s Status) error {
	c.log("CreateStatus", org, repo, sha, s)
	_, err := c.request(&request{
		method:      http.MethodPost,
		path:        fmt.Sprintf("%s/repos/%s/%s/statuses/%s", c.base, org, repo, sha),
		requestBody: &s,
		exitCodes:   []int{201},
	}, nil)
	return err
}

// ListStatuses gets commit statuses for a given ref.
func (c *Client) ListStatuses(org, repo, ref string) ([]Status, error) {
	c.log("ListStatuses", org, repo, ref)
	var statuses []Status
	_, err := c.request(&request{
		method:    http.MethodGet,
		path:      fmt.Sprintf("%s/repos/%s/%s/statuses/%s", c.base, org, repo, ref),
		exitCodes: []int{200},
	}, &statuses)
	return statuses, err
}

// GetRepo returns the repo for the provided owner/name combination.
func (c *Client) GetRepo(owner, name string) (Repo, error) {
	c.log("GetRepo", owner, name)

	var repo Repo
	_, err := c.request(&request{
		method:    http.MethodGet,
		path:      fmt.Sprintf("%s/repos/%s/%s", c.base, owner, name),
		exitCodes: []int{200},
	}, &repo)
	return repo, err
}

func (c *Client) GetRepos(org string, isUser bool) ([]Repo, error) {
	c.log("GetRepos", org, isUser)
	var (
		repos   []Repo
		nextURL string
	)
	if c.fake {
		return repos, nil
	}
	if isUser {
		nextURL = fmt.Sprintf("%s/users/%s/repos", c.base, org)
	} else {
		nextURL = fmt.Sprintf("%s/orgs/%s/repos", c.base, org)
	}
	for nextURL != "" {
		resp, err := c.requestRetry(http.MethodGet, nextURL, "", nil)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, fmt.Errorf("return code not 2XX: %s", resp.Status)
		}

		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		var reps []Repo
		if err := json.Unmarshal(b, &reps); err != nil {
			return nil, err
		}
		repos = append(repos, reps...)
		nextURL = parseLinks(resp.Header.Get("Link"))["next"]
	}
	return repos, nil
}

func (c *Client) GetBranches(org, repo string) ([]Branch, error) {
	c.log("GetBranches", org, repo)
	var branches []Branch
	_, err := c.request(&request{
		method:    http.MethodGet,
		path:      fmt.Sprintf("%s/repos/%s/%s/branches", c.base, org, repo),
		exitCodes: []int{200},
	}, &branches)
	return branches, err
}

func (c *Client) RemoveBranchProtection(org, repo, branch string) error {
	c.log("RemoveBranchProtection", org, repo, branch)
	_, err := c.request(&request{
		method:    http.MethodDelete,
		path:      fmt.Sprintf("%s/repos/%s/%s/branches/%s/protection", c.base, org, repo, branch),
		exitCodes: []int{204},
	}, nil)
	return err
}

func (c *Client) UpdateBranchProtection(org, repo, branch string, config BranchProtectionRequest) error {
	c.log("UpdateBranchProtection", org, repo, branch, config)
	_, err := c.request(&request{
		method:      http.MethodPut,
		path:        fmt.Sprintf("%s/repos/%s/%s/branches/%s/protection", c.base, org, repo, branch),
		requestBody: config,
		exitCodes:   []int{200},
	}, nil)
	return err
}

// Adds Label label/color to given org/repo
func (c *Client) AddRepoLabel(org, repo, label, color string) error {
	c.log("AddRepoLabel", org, repo, label, color)
	_, err := c.request(&request{
		method:      http.MethodPost,
		path:        fmt.Sprintf("%s/repos/%s/%s/labels", c.base, org, repo),
		requestBody: Label{Name: label, Color: color},
		exitCodes:   []int{201},
	}, nil)
	return err
}

// Updates org/repo label to new name and color
func (c *Client) UpdateRepoLabel(org, repo, label, name, color string) error {
	c.log("UpdateRepoLabel", org, repo, label, name, color)
	_, err := c.request(&request{
		method:      http.MethodPatch,
		path:        fmt.Sprintf("%s/repos/%s/%s/labels/%s", c.base, org, repo, label),
		requestBody: Label{Name: name, Color: color},
		exitCodes:   []int{200},
	}, nil)
	return err
}

// Delete label in org/repo
func (c *Client) DeleteRepoLabel(org, repo, label string) error {
	c.log("DeleteRepoLabel", org, repo, label)
	_, err := c.request(&request{
		method:      http.MethodDelete,
		path:        fmt.Sprintf("%s/repos/%s/%s/labels/%s", c.base, org, repo, label),
		requestBody: Label{Name: label},
		exitCodes:   []int{204},
	}, nil)
	return err
}

// GetCombinedStatus returns the latest statuses for a given ref.
func (c *Client) GetCombinedStatus(org, repo, ref string) (*CombinedStatus, error) {
	c.log("GetCombinedStatus", org, repo, ref)
	var combinedStatus CombinedStatus
	_, err := c.request(&request{
		method:    http.MethodGet,
		path:      fmt.Sprintf("%s/repos/%s/%s/commits/%s/status", c.base, org, repo, ref),
		exitCodes: []int{200},
	}, &combinedStatus)
	return &combinedStatus, err
}

// getLabels is a helper function that retrieves a paginated list of labels from a github URI path.
func (c *Client) getLabels(path string) ([]Label, error) {
	var labels []Label
	if c.fake {
		return labels, nil
	}
	err := c.readPaginatedResults(
		path,
		"",
		func() interface{} {
			return &[]Label{}
		},
		func(obj interface{}) {
			labels = append(labels, *(obj.(*[]Label))...)
		},
	)
	if err != nil {
		return nil, err
	}
	return labels, nil
}

func (c *Client) GetRepoLabels(org, repo string) ([]Label, error) {
	c.log("GetRepoLabels", org, repo)
	return c.getLabels(fmt.Sprintf("/repos/%s/%s/labels", org, repo))
}

func (c *Client) GetIssueLabels(org, repo string, number int) ([]Label, error) {
	c.log("GetIssueLabels", org, repo, number)
	return c.getLabels(fmt.Sprintf("/repos/%s/%s/issues/%d/labels", org, repo, number))
}

func (c *Client) AddLabel(org, repo string, number int, label string) error {
	c.log("AddLabel", org, repo, number, label)
	_, err := c.request(&request{
		method:      http.MethodPost,
		path:        fmt.Sprintf("%s/repos/%s/%s/issues/%d/labels", c.base, org, repo, number),
		requestBody: []string{label},
		exitCodes:   []int{200},
	}, nil)
	return err
}

// LabelNotFound indicates that a label is not attached to an issue. For example, removing a
// label from an issue, when the issue does not have that label.
type LabelNotFound struct {
	Owner, Repo string
	Number      int
	Label       string
}

func (e *LabelNotFound) Error() string {
	return fmt.Sprintf("label %q does not exist on %s/%s/%d", e.Label, e.Owner, e.Repo, e.Number)
}

type githubError struct {
	Message string `json:"message,omitempty"`
}

func (c *Client) RemoveLabel(org, repo string, number int, label string) error {
	c.log("RemoveLabel", org, repo, number, label)
	code, body, err := c.requestRaw(&request{
		method: http.MethodDelete,
		path:   fmt.Sprintf("%s/repos/%s/%s/issues/%d/labels/%s", c.base, org, repo, number, label),
		// GitHub sometimes returns 200 for this call, which is a bug on their end.
		// Do not expect a 404 exit code and handle it separately because we need
		// to introspect the request's response body.
		exitCodes: []int{200, 204},
	})

	switch {
	case code == 200 || code == 204:
		// If our code was 200 or 204, no error info.
		return nil
	case code == 404:
		// continue
	case err != nil:
		return err
	default:
		return fmt.Errorf("unexpected status code: %v", code)
	}

	ge := &githubError{}
	if err := json.Unmarshal(body, ge); err != nil {
		return err
	}

	// If the error was because the label was not found, annotate that error with type information.
	if ge.Message == "Label does not exist" {
		return &LabelNotFound{
			Owner:  org,
			Repo:   repo,
			Number: number,
			Label:  label,
		}
	}

	// Otherwise we got some other 404 error.
	return fmt.Errorf("deleting label 404: %s", ge.Message)
}

type MissingUsers struct {
	Users  []string
	action string
}

func (m MissingUsers) Error() string {
	return fmt.Sprintf("could not %s the following user(s): %s.", m.action, strings.Join(m.Users, ", "))
}

func (c *Client) AssignIssue(org, repo string, number int, logins []string) error {
	c.log("AssignIssue", org, repo, number, logins)
	assigned := make(map[string]bool)
	var i Issue
	_, err := c.request(&request{
		method:      http.MethodPost,
		path:        fmt.Sprintf("%s/repos/%s/%s/issues/%d/assignees", c.base, org, repo, number),
		requestBody: map[string][]string{"assignees": logins},
		exitCodes:   []int{201},
	}, &i)
	if err != nil {
		return err
	}
	for _, assignee := range i.Assignees {
		assigned[NormLogin(assignee.Login)] = true
	}
	missing := MissingUsers{action: "assign"}
	for _, login := range logins {
		if !assigned[NormLogin(login)] {
			missing.Users = append(missing.Users, login)
		}
	}
	if len(missing.Users) > 0 {
		return missing
	}
	return nil
}

type ExtraUsers struct {
	Users  []string
	action string
}

func (e ExtraUsers) Error() string {
	return fmt.Sprintf("could not %s the following user(s): %s.", e.action, strings.Join(e.Users, ", "))
}

func (c *Client) UnassignIssue(org, repo string, number int, logins []string) error {
	c.log("UnassignIssue", org, repo, number, logins)
	assigned := make(map[string]bool)
	var i Issue
	_, err := c.request(&request{
		method:      http.MethodDelete,
		path:        fmt.Sprintf("%s/repos/%s/%s/issues/%d/assignees", c.base, org, repo, number),
		requestBody: map[string][]string{"assignees": logins},
		exitCodes:   []int{200},
	}, &i)
	if err != nil {
		return err
	}
	for _, assignee := range i.Assignees {
		assigned[NormLogin(assignee.Login)] = true
	}
	extra := ExtraUsers{action: "unassign"}
	for _, login := range logins {
		if assigned[NormLogin(login)] {
			extra.Users = append(extra.Users, login)
		}
	}
	if len(extra.Users) > 0 {
		return extra
	}
	return nil
}

// CreateReview creates a review using the draft.
func (c *Client) CreateReview(org, repo string, number int, r DraftReview) error {
	c.log("CreateReview", org, repo, number, r)
	_, err := c.request(&request{
		method:      http.MethodPost,
		path:        fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews", c.base, org, repo, number),
		accept:      "application/vnd.github.black-cat-preview+json",
		requestBody: r,
		exitCodes:   []int{200},
	}, nil)
	return err
}

func (c *Client) tryRequestReview(org, repo string, number int, logins []string) (int, error) {
	c.log("RequestReview", org, repo, number, logins)
	var pr PullRequest
	return c.request(&request{
		method:      http.MethodPost,
		path:        fmt.Sprintf("%s/repos/%s/%s/pulls/%d/requested_reviewers", c.base, org, repo, number),
		accept:      "application/vnd.github.black-cat-preview+json",
		requestBody: map[string][]string{"reviewers": logins},
		exitCodes:   []int{http.StatusCreated /*201*/},
	}, &pr)
}

// RequestReview tries to add the users listed in 'logins' as requested reviewers of the specified PR.
// If any user in the 'logins' slice is not a contributor of the repo, the entire POST will fail
// without adding any reviewers. The github API response does not specify which user(s) were invalid
// so if we fail to request reviews from the members of 'logins' we try to request reviews from
// each member individually. We try first with all users in 'logins' for efficiency in the common case.
func (c *Client) RequestReview(org, repo string, number int, logins []string) error {
	statusCode, err := c.tryRequestReview(org, repo, number, logins)
	if err != nil && statusCode == http.StatusUnprocessableEntity /*422*/ {
		// Failed to set all members of 'logins' as reviewers, try individually.
		missing := MissingUsers{action: "request a PR review from"}
		for _, user := range logins {
			statusCode, err = c.tryRequestReview(org, repo, number, []string{user})
			if err != nil && statusCode == http.StatusUnprocessableEntity /*422*/ {
				// User is not a contributor.
				missing.Users = append(missing.Users, user)
			} else if err != nil {
				return fmt.Errorf("failed to add reviewer to PR. Status code: %d, errmsg: %v", statusCode, err)
			}
		}
		if len(missing.Users) > 0 {
			return missing
		}
		return nil
	}
	return err
}

// UnrequestReview tries to remove the users listed in 'logins' from the requested reviewers of the
// specified PR. The github API treats deletions of review requests differently than creations. Specifically, if
// 'logins' contains a user that isn't a requested reviewer, other users that are valid are still removed.
// Furthermore, the API response lists the set of requested reviewers after the deletion (unlike request creations),
// so we can determine if each deletion was successful.
// The API responds with http status code 200 no matter what the content of 'logins' is.
func (c *Client) UnrequestReview(org, repo string, number int, logins []string) error {
	c.log("UnrequestReview", org, repo, number, logins)
	var pr PullRequest
	_, err := c.request(&request{
		method:      http.MethodDelete,
		path:        fmt.Sprintf("%s/repos/%s/%s/pulls/%d/requested_reviewers", c.base, org, repo, number),
		accept:      "application/vnd.github.black-cat-preview+json",
		requestBody: map[string][]string{"reviewers": logins},
		exitCodes:   []int{http.StatusOK /*200*/},
	}, &pr)
	if err != nil {
		return err
	}
	extras := ExtraUsers{action: "remove the PR review request for"}
	for _, user := range pr.RequestedReviewers {
		found := false
		for _, toDelete := range logins {
			if NormLogin(user.Login) == NormLogin(toDelete) {
				found = true
				break
			}
		}
		if found {
			extras.Users = append(extras.Users, user.Login)
		}
	}
	if len(extras.Users) > 0 {
		return extras
	}
	return nil
}

// CloseIssue closes the existing, open issue provided
func (c *Client) CloseIssue(org, repo string, number int) error {
	c.log("CloseIssue", org, repo, number)
	_, err := c.request(&request{
		method:      http.MethodPatch,
		path:        fmt.Sprintf("%s/repos/%s/%s/issues/%d", c.base, org, repo, number),
		requestBody: map[string]string{"state": "closed"},
		exitCodes:   []int{200},
	}, nil)
	return err
}

// StateCannotBeChanged represents the "custom" GitHub API
// error that occurs when a resource cannot be changed
type StateCannotBeChanged struct {
	Message string
}

func (s StateCannotBeChanged) Error() string {
	return s.Message
}

// StateCannotBeChanged implements error
var _ error = (*StateCannotBeChanged)(nil)

// convert to a StateCannotBeChanged if appropriate or else return the original error
func stateCannotBeChangedOrOriginalError(err error) error {
	requestErr, ok := err.(requestError)
	if ok && len(requestErr.Errors) > 0 {
		for _, subErr := range requestErr.Errors {
			if strings.Contains(subErr.Message, stateCannotBeChangedMessagePrefix) {
				return StateCannotBeChanged{
					Message: subErr.Message,
				}
			}
		}
	}
	return err
}

// ReopenIssue re-opens the existing, closed issue provided
func (c *Client) ReopenIssue(org, repo string, number int) error {
	c.log("ReopenIssue", org, repo, number)
	_, err := c.request(&request{
		method:      http.MethodPatch,
		path:        fmt.Sprintf("%s/repos/%s/%s/issues/%d", c.base, org, repo, number),
		requestBody: map[string]string{"state": "open"},
		exitCodes:   []int{200},
	}, nil)
	return stateCannotBeChangedOrOriginalError(err)
}

// ClosePR closes the existing, open PR provided
// TODO: Rename to ClosePullRequest
func (c *Client) ClosePR(org, repo string, number int) error {
	c.log("ClosePR", org, repo, number)
	_, err := c.request(&request{
		method:      http.MethodPatch,
		path:        fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.base, org, repo, number),
		requestBody: map[string]string{"state": "closed"},
		exitCodes:   []int{200},
	}, nil)
	return err
}

// ReopenPR re-opens the existing, closed PR provided
// TODO: Rename to ReopenPullRequest
func (c *Client) ReopenPR(org, repo string, number int) error {
	c.log("ReopenPR", org, repo, number)
	_, err := c.request(&request{
		method:      http.MethodPatch,
		path:        fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.base, org, repo, number),
		requestBody: map[string]string{"state": "open"},
		exitCodes:   []int{200},
	}, nil)
	return stateCannotBeChangedOrOriginalError(err)
}

// GetRef returns the SHA of the given ref, such as "heads/master".
func (c *Client) GetRef(org, repo, ref string) (string, error) {
	c.log("GetRef", org, repo, ref)
	var res struct {
		Object map[string]string `json:"object"`
	}
	_, err := c.request(&request{
		method:    http.MethodGet,
		path:      fmt.Sprintf("%s/repos/%s/%s/git/refs/%s", c.base, org, repo, ref),
		exitCodes: []int{200},
	}, &res)
	return res.Object["sha"], err
}

// FindIssues uses the github search API to find issues which match a particular query.
//
// Input query the same way you would into the website.
// Order returned results with sort (usually "updated").
// Control whether oldest/newest is first with asc.
//
// See https://help.github.com/articles/searching-issues-and-pull-requests/ for details.
func (c *Client) FindIssues(query, sort string, asc bool) ([]Issue, error) {
	c.log("FindIssues", query)
	path := fmt.Sprintf("%s/search/issues?q=%s", c.base, url.QueryEscape(query))
	if sort != "" {
		path += "&sort=" + url.QueryEscape(sort)
		if asc {
			path += "&order=asc"
		}
	}
	var issSearchResult IssuesSearchResult
	_, err := c.request(&request{
		method:    http.MethodGet,
		path:      path,
		exitCodes: []int{200},
	}, &issSearchResult)
	return issSearchResult.Issues, err
}

type FileNotFound struct {
	org, repo, path, commit string
}

func (e *FileNotFound) Error() string {
	return fmt.Sprintf("%s/%s/%s @ %s not found", e.org, e.repo, e.path, e.commit)
}

// GetFile uses github repo contents API to retrieve the content of a file with commit sha.
// If commit is empty, it will grab content from repo's default branch, usually master.
// TODO(krzyzacy): Support retrieve a directory
func (c *Client) GetFile(org, repo, filepath, commit string) ([]byte, error) {
	c.log("GetFile", org, repo, filepath, commit)

	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", c.base, org, repo, filepath)
	if commit != "" {
		url = fmt.Sprintf("%s?ref=%s", url, commit)
	}

	var res Content
	code, err := c.request(&request{
		method:    http.MethodGet,
		path:      url,
		exitCodes: []int{200, 404},
	}, &res)

	if err != nil {
		return nil, err
	}

	if code == 404 {
		return nil, &FileNotFound{
			org:    org,
			repo:   repo,
			path:   filepath,
			commit: commit,
		}
	}

	decoded, err := base64.StdEncoding.DecodeString(res.Content)
	if err != nil {
		return nil, fmt.Errorf("error decoding %s : %v", res.Content, err)
	}

	return decoded, nil
}

// Query runs a GraphQL query using shurcooL/githubql's client.
func (c *Client) Query(ctx context.Context, q interface{}, vars map[string]interface{}) error {
	// Don't log query here because Query is typically called multiple times to get all pages.
	// Instead log once per search and include total search cost.
	return c.gqlc.Query(ctx, q, vars)
}

// ListTeams gets a list of teams for the given org
func (c *Client) ListTeams(org string) ([]Team, error) {
	c.log("ListTeams", org)
	if c.fake {
		return nil, nil
	}
	path := fmt.Sprintf("/orgs/%s/teams", org)
	var teams []Team
	err := c.readPaginatedResults(
		path,
		"",
		func() interface{} {
			return &[]Team{}
		},
		func(obj interface{}) {
			teams = append(teams, *(obj.(*[]Team))...)
		},
	)
	if err != nil {
		return nil, err
	}
	return teams, nil
}

// ListTeamMembers gets a list of team members for the given team id
func (c *Client) ListTeamMembers(id int) ([]TeamMember, error) {
	c.log("ListTeamMembers", id)
	if c.fake {
		return nil, nil
	}
	path := fmt.Sprintf("/teams/%d/members", id)
	var teamMembers []TeamMember
	err := c.readPaginatedResults(
		path,
		// This accept header enables the nested teams preview.
		// https://developer.github.com/changes/2017-08-30-preview-nested-teams/
		"application/vnd.github.hellcat-preview+json",
		func() interface{} {
			return &[]TeamMember{}
		},
		func(obj interface{}) {
			teamMembers = append(teamMembers, *(obj.(*[]TeamMember))...)
		},
	)
	if err != nil {
		return nil, err
	}
	return teamMembers, nil
}

// MergeDetails contains desired properties of the merge.
// See https://developer.github.com/v3/pulls/#merge-a-pull-request-merge-button
type MergeDetails struct {
	// CommitTitle defaults to the automatic message.
	CommitTitle string `json:"commit_title,omitempty"`
	// CommitMessage defaults to the automatic message.
	CommitMessage string `json:"commit_message,omitempty"`
	// The PR HEAD must match this to prevent races.
	SHA string `json:"sha,omitempty"`
	// Can be "merge", "squash", or "rebase". Defaults to merge.
	MergeMethod string `json:"merge_method,omitempty"`
}

type ModifiedHeadError string

func (e ModifiedHeadError) Error() string { return string(e) }

type UnmergablePRError string

func (e UnmergablePRError) Error() string { return string(e) }

type UnmergablePRBaseChangedError string

func (e UnmergablePRBaseChangedError) Error() string { return string(e) }

type UnauthorizedToPushError string

func (e UnauthorizedToPushError) Error() string { return string(e) }

// Merge merges a PR.
func (c *Client) Merge(org, repo string, pr int, details MergeDetails) error {
	c.log("Merge", org, repo, pr, details)
	ge := githubError{}
	ec, err := c.request(&request{
		method:      http.MethodPut,
		path:        fmt.Sprintf("%s/repos/%s/%s/pulls/%d/merge", c.base, org, repo, pr),
		requestBody: &details,
		exitCodes:   []int{200, 405, 409},
	}, &ge)
	if err != nil {
		return err
	}
	if ec == 405 {
		if strings.Contains(ge.Message, "Base branch was modified") {
			return UnmergablePRBaseChangedError(ge.Message)
		}
		if strings.Contains(ge.Message, "You're not authorized to push to this branch") {
			return UnauthorizedToPushError(ge.Message)
		}
		return UnmergablePRError(ge.Message)
	} else if ec == 409 {
		return ModifiedHeadError(ge.Message)
	}

	return nil
}

// ListCollaborators gets a list of all users who have access to a repo (and can become assignees
// or requested reviewers). This includes, org members with access, outside collaborators, and org
// owners.
func (c *Client) ListCollaborators(org, repo string) ([]User, error) {
	c.log("ListCollaborators", org, repo)
	if c.fake {
		return nil, nil
	}
	path := fmt.Sprintf("/repos/%s/%s/collaborators", org, repo)
	var users []User
	err := c.readPaginatedResults(
		path,
		// This accept header enables the nested teams preview.
		// https://developer.github.com/changes/2017-08-30-preview-nested-teams/
		"application/vnd.github.hellcat-preview+json",
		func() interface{} {
			return &[]User{}
		},
		func(obj interface{}) {
			users = append(users, *(obj.(*[]User))...)
		},
	)
	if err != nil {
		return nil, err
	}
	return users, nil
}

// CreateFork creates a fork for the authenticated user. Forking a repository
// happens asynchronously. Therefore, we may have to wait a short period before
// accessing the git objects. If this takes longer than 5 minutes, Github
// recommends contacting their support.
//
// See https://developer.github.com/v3/repos/forks/#create-a-fork
func (c *Client) CreateFork(owner, repo string) error {
	c.log("CreateFork", owner, repo)
	_, err := c.request(&request{
		method:    http.MethodPost,
		path:      fmt.Sprintf("%s/repos/%s/%s/forks", c.base, owner, repo),
		exitCodes: []int{202},
	}, nil)
	return err
}

// ListIssueEvents gets a list events from github's events API that pertain to the specified issue.
// The events that are returned have a different format than webhook events and certain event types
// are excluded.
// https://developer.github.com/v3/issues/events/
func (c *Client) ListIssueEvents(org, repo string, num int) ([]ListedIssueEvent, error) {
	c.log("ListIssueEvents", org, repo, num)
	if c.fake {
		return nil, nil
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/events", org, repo, num)
	var events []ListedIssueEvent
	err := c.readPaginatedResults(
		path,
		"",
		func() interface{} {
			return &[]ListedIssueEvent{}
		},
		func(obj interface{}) {
			events = append(events, *(obj.(*[]ListedIssueEvent))...)
		},
	)
	if err != nil {
		return nil, err
	}
	return events, nil
}

// IsMergeable determines if a PR can be merged.
// Mergeability is calculated by a background job on github and is not immediately available when
// new commits are added so the PR must be polled until the background job completes.
func (c *Client) IsMergeable(org, repo string, number int, sha string) (bool, error) {
	backoff := time.Second * 3
	maxTries := 3
	for try := 0; try < maxTries; try++ {
		pr, err := c.GetPullRequest(org, repo, number)
		if err != nil {
			return false, err
		}
		if pr.Head.SHA != sha {
			return false, fmt.Errorf("pull request head changed while checking mergeability (%s -> %s)", sha, pr.Head.SHA)
		}
		if pr.Merged {
			return false, errors.New("pull request was merged while checking mergeability")
		}
		if pr.Mergable != nil {
			return *pr.Mergable, nil
		}
		if try+1 < maxTries {
			c.time.Sleep(backoff)
			backoff *= 2
		}
	}
	return false, fmt.Errorf("reached maximum number of retries (%d) checking mergeability", maxTries)
}

// ClearMilestone clears the milestone from the specified issue
func (c *Client) ClearMilestone(org, repo string, num int) error {
	c.log("ClearMilestone", org, repo, num)

	issue := &struct {
		// Clearing the milestone requires providing a null value, and
		// interface{} will serialize to null.
		Milestone interface{} `json:"milestone"`
	}{}
	_, err := c.request(&request{
		method:      http.MethodPatch,
		path:        fmt.Sprintf("%s/repos/%v/%v/issues/%d", c.base, org, repo, num),
		requestBody: &issue,
		exitCodes:   []int{200},
	}, nil)
	return err
}

// SetMilestone sets the milestone from the specified issue (if it is a valid milestone)
func (c *Client) SetMilestone(org, repo string, issueNum, milestoneNum int) error {
	c.log("SetMilestone", org, repo, issueNum, milestoneNum)

	issue := &struct {
		Milestone int `json:"milestone"`
	}{Milestone: milestoneNum}

	_, err := c.request(&request{
		method:      http.MethodPatch,
		path:        fmt.Sprintf("%s/repos/%v/%v/issues/%d", c.base, org, repo, issueNum),
		requestBody: &issue,
		exitCodes:   []int{200},
	}, nil)
	return err
}

// ListMilestones list all milestones in a repo
// https://developer.github.com/v3/issues/milestones/#list-milestones-for-a-repository/
func (c *Client) ListMilestones(org, repo string) ([]Milestone, error) {
	c.log("ListMilestones", org)
	if c.fake {
		return nil, nil
	}
	path := fmt.Sprintf("/repos/%s/%s/milestones", org, repo)
	var milestones []Milestone
	err := c.readPaginatedResults(
		path,
		"",
		func() interface{} {
			return &[]Milestone{}
		},
		func(obj interface{}) {
			milestones = append(milestones, *(obj.(*[]Milestone))...)
		},
	)
	if err != nil {
		return nil, err
	}
	return milestones, nil
}
