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
	"reflect"
	"sort"
	"testing"

	"k8s.io/test-infra/prow/config/org"
	"k8s.io/test-infra/prow/flagutil"
	"k8s.io/test-infra/prow/github"

	"k8s.io/apimachinery/pkg/util/sets"
)

func TestOptions(t *testing.T) {
	weirdFlags := flagutil.NewStrings(defaultEndpoint)
	weirdFlags.Set("weird://url") // no error possible
	cases := []struct {
		name     string
		args     []string
		expected *options
	}{
		{
			name: "missing --config",
			args: []string{"--github-token-path=fake"},
		},
		{
			name: "missing --github-token-path",
			args: []string{"--config-path=fake"},
		},
		{
			name: "bad --github-endpoint",
			args: []string{"--config-path=foo", "--github-token-path=bar", "--github-endpoint=ht!tp://:dumb"},
		},
		{
			name: "--minAdmins too low",
			args: []string{"--config-path=foo", "--github-token-path=bar", "--min-admins=1"},
		},
		{
			name: "--maximum-removal-delta too high",
			args: []string{"--config-path=foo", "--github-token-path=bar", "--maximum-removal-delta=1.1"},
		},
		{
			name: "--maximum-removal-delta too low",
			args: []string{"--config-path=foo", "--github-token-path=bar", "--maximum-removal-delta=-0.1"},
		},
		{
			name: "maximal delta",
			args: []string{"--config-path=foo", "--github-token-path=bar", "--maximum-removal-delta=1"},
			expected: &options{
				config:       "foo",
				token:        "bar",
				endpoint:     flagutil.NewStrings(defaultEndpoint),
				minAdmins:    defaultMinAdmins,
				requireSelf:  true,
				maximumDelta: 1,
			},
		},
		{
			name: "minimal delta",
			args: []string{"--config-path=foo", "--github-token-path=bar", "--maximum-removal-delta=0"},
			expected: &options{
				config:       "foo",
				token:        "bar",
				endpoint:     flagutil.NewStrings(defaultEndpoint),
				minAdmins:    defaultMinAdmins,
				requireSelf:  true,
				maximumDelta: 0,
			},
		},
		{
			name: "minimal admins",
			args: []string{"--config-path=foo", "--github-token-path=bar", "--min-admins=2"},
			expected: &options{
				config:       "foo",
				token:        "bar",
				endpoint:     flagutil.NewStrings(defaultEndpoint),
				minAdmins:    2,
				requireSelf:  true,
				maximumDelta: defaultDelta,
			},
		},
		{
			name: "minimal",
			args: []string{"--config-path=foo", "--github-token-path=bar"},
			expected: &options{
				config:       "foo",
				token:        "bar",
				endpoint:     flagutil.NewStrings(defaultEndpoint),
				minAdmins:    defaultMinAdmins,
				requireSelf:  true,
				maximumDelta: defaultDelta,
			},
		},
		{
			name: "full",
			args: []string{"--config-path=foo", "--github-token-path=bar", "--github-endpoint=weird://url", "--confirm=true", "--require-self=false"},
			expected: &options{
				config:       "foo",
				token:        "bar",
				endpoint:     weirdFlags,
				confirm:      true,
				requireSelf:  false,
				minAdmins:    defaultMinAdmins,
				maximumDelta: defaultDelta,
			},
		},
	}

	for _, tc := range cases {
		flags := flag.NewFlagSet(tc.name, flag.ContinueOnError)
		var actual options
		err := actual.parseArgs(flags, tc.args)
		switch {
		case err == nil && tc.expected == nil:
			t.Errorf("%s: failed to return an error", tc.name)
		case err != nil && tc.expected != nil:
			t.Errorf("%s: unexpected error: %v", tc.name, err)
		case tc.expected != nil && !reflect.DeepEqual(*tc.expected, actual):
			t.Errorf("%s: actual %v != expected %v", tc.name, actual, *tc.expected)
		}
	}
}

type fakeClient struct {
	orgMembers sets.String
	admins     sets.String
	members    sets.String
	removed    sets.String
	newAdmins  sets.String
	newMembers sets.String
}

func (c *fakeClient) BotName() (string, error) {
	return "me", nil
}

func (c fakeClient) makeMembers(people sets.String) []github.TeamMember {
	var ret []github.TeamMember
	for p := range people {
		ret = append(ret, github.TeamMember{Login: p})
	}
	return ret
}

func (c *fakeClient) ListOrgMembers(org, role string) ([]github.TeamMember, error) {
	switch role {
	case github.RoleMember:
		return c.makeMembers(c.members), nil
	case github.RoleAdmin:
		return c.makeMembers(c.admins), nil
	default:
		// RoleAll: implmenent when/if necessary
		return nil, fmt.Errorf("bad role: %s", role)
	}
}

func (c *fakeClient) RemoveOrgMembership(org, user string) error {
	if user == "fail" {
		return fmt.Errorf("injected failure for %s", user)
	}
	c.removed.Insert(user)
	c.admins.Delete(user)
	c.members.Delete(user)
	return nil
}

func (c *fakeClient) UpdateOrgMembership(org, user string, admin bool) (*github.OrgMembership, error) {
	if user == "fail" {
		return nil, fmt.Errorf("injected failure for %s", user)
	}
	var state string
	if c.members.Has(user) || c.admins.Has(user) {
		state = github.StateActive
	} else {
		state = github.StatePending
	}
	var role string
	if admin {
		c.newAdmins.Insert(user)
		c.admins.Insert(user)
		role = github.RoleAdmin
	} else {
		c.newMembers.Insert(user)
		c.members.Insert(user)
		role = github.RoleMember
	}
	return &github.OrgMembership{
		Membership: github.Membership{
			Role:  role,
			State: state,
		},
	}, nil
}

func (c *fakeClient) ListTeamMembers(id int, role string) ([]github.TeamMember, error) {
	if id != teamID {
		return nil, fmt.Errorf("only team 66 supported, not %d", id)
	}
	switch role {
	case github.RoleMember:
		return c.makeMembers(c.members), nil
	case github.RoleMaintainer:
		return c.makeMembers(c.admins), nil
	default:
		return nil, fmt.Errorf("fake does not support: %v", role)
	}
}

const teamID = 66

func (c *fakeClient) UpdateTeamMembership(id int, user string, maintainer bool) (*github.TeamMembership, error) {
	if id != teamID {
		return nil, fmt.Errorf("only team %d supported, not %d", teamID, id)
	}
	if user == "fail" {
		return nil, fmt.Errorf("injected failure for %s", user)
	}
	var state string
	if c.orgMembers.Has(user) || len(c.orgMembers) == 0 {
		state = github.StateActive
	} else {
		state = github.StatePending
	}
	var role string
	if maintainer {
		c.newAdmins.Insert(user)
		c.admins.Insert(user)
		role = github.RoleMaintainer
	} else {
		c.newMembers.Insert(user)
		c.members.Insert(user)
		role = github.RoleMember
	}
	return &github.TeamMembership{
		Membership: github.Membership{
			Role:  role,
			State: state,
		},
	}, nil
}

func (c *fakeClient) RemoveTeamMembership(id int, user string) error {
	if id != teamID {
		return fmt.Errorf("only team %d supported, not %d", teamID, id)
	}
	if user == "fail" {
		return fmt.Errorf("injected failure for %s", user)
	}
	c.removed.Insert(user)
	c.admins.Delete(user)
	c.members.Delete(user)
	return nil
}

func TestConfigureMembers(t *testing.T) {
	cases := []struct {
		name    string
		want    memberships
		have    memberships
		remove  sets.String
		members sets.String
		supers  sets.String
		err     bool
	}{
		{
			name: "forgot to remove duplicate entry",
			want: memberships{
				members: sets.NewString("me"),
				super:   sets.NewString("me"),
			},
			err: true,
		},
		{
			name: "removal fails",
			have: memberships{
				members: sets.NewString("fail"),
			},
			err: true,
		},
		{
			name: "adding admin fails",
			want: memberships{
				super: sets.NewString("fail"),
			},
			err: true,
		},
		{
			name: "adding member fails",
			want: memberships{
				members: sets.NewString("fail"),
			},
			err: true,
		},
		{
			name: "promote to admin",
			have: memberships{
				members: sets.NewString("promote"),
			},
			want: memberships{
				super: sets.NewString("promote"),
			},
			supers: sets.NewString("promote"),
		},
		{
			name: "downgrade to member",
			have: memberships{
				super: sets.NewString("downgrade"),
			},
			want: memberships{
				members: sets.NewString("downgrade"),
			},
			members: sets.NewString("downgrade"),
		},
		{
			name: "some of everything",
			have: memberships{
				super:   sets.NewString("keep-admin", "drop-admin"),
				members: sets.NewString("keep-member", "drop-member"),
			},
			want: memberships{
				members: sets.NewString("keep-member", "new-member"),
				super:   sets.NewString("keep-admin", "new-admin"),
			},
			remove:  sets.NewString("drop-admin", "drop-member"),
			members: sets.NewString("new-member"),
			supers:  sets.NewString("new-admin"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			removed := sets.String{}
			members := sets.String{}
			supers := sets.String{}
			adder := func(user string, super bool) error {
				if user == "fail" {
					return fmt.Errorf("injected adder failure for %s", user)
				}
				if super {
					supers.Insert(user)
				} else {
					members.Insert(user)
				}
				return nil
			}

			remover := func(user string) error {
				if user == "fail" {
					return fmt.Errorf("injected remover failure for %s", user)
				}
				removed.Insert(user)
				return nil
			}

			err := configureMembers(tc.have, tc.want, adder, remover)
			switch {
			case err != nil:
				if !tc.err {
					t.Errorf("Unexpected error: %v", err)
				}
			case tc.err:
				t.Errorf("Failed to receive error")
			default:
				if err := cmpLists(tc.remove.List(), removed.List()); err != nil {
					t.Errorf("Wrong users removed: %v", err)
				} else if err := cmpLists(tc.members.List(), members.List()); err != nil {
					t.Errorf("Wrong members added: %v", err)
				} else if err := cmpLists(tc.supers.List(), supers.List()); err != nil {
					t.Errorf("Wrong supers added: %v", err)
				}
			}
		})
	}
}

func TestConfigureOrgMembers(t *testing.T) {
	cases := []struct {
		name       string
		opt        options
		config     org.Config
		admins     []string
		members    []string
		err        bool
		remove     []string
		addAdmins  []string
		addMembers []string
	}{
		{
			name: "too few admins",
			opt: options{
				minAdmins: 5,
			},
			config: org.Config{
				Admins: []string{"joe"},
			},
			err: true,
		},
		{
			name: "remove too many admins",
			opt: options{
				maximumDelta: 0.3,
			},
			config: org.Config{
				Admins: []string{"keep", "me"},
			},
			admins: []string{"a", "b", "c", "keep"},
			err:    true,
		},
		{
			name: "forgot to add self",
			opt: options{
				requireSelf: true,
			},
			config: org.Config{
				Admins: []string{"other"},
			},
			err: true,
		},
		{
			name: "forgot to add required admins",
			opt: options{
				requiredAdmins: flagutil.NewStrings("francis"),
			},
			err: true,
		},
		{
			name:   "can remove self with flag",
			config: org.Config{},
			opt: options{
				maximumDelta: 1,
				requireSelf:  false,
			},
			admins: []string{"me"},
			remove: []string{"me"},
		},
		{
			name: "reject same person with both roles",
			config: org.Config{
				Admins:  []string{"me"},
				Members: []string{"me"},
			},
			err: true,
		},
		{
			name:   "github remove rpc fails",
			admins: []string{"fail"},
			err:    true,
		},
		{
			name: "github add rpc fails",
			config: org.Config{
				Admins: []string{"fail"},
			},
			err: true,
		},
		{
			name: "require team member to be org member",
			config: org.Config{
				Teams: map[string]org.Team{
					"group": {
						Members: []string{"non-member"},
					},
				},
			},
			err: true,
		},
		{
			name: "require team maintainer to be org member",
			config: org.Config{
				Teams: map[string]org.Team{
					"group": {
						Maintainers: []string{"non-member"},
					},
				},
			},
			err: true,
		},
		{
			name: "disallow duplicate names",
			config: org.Config{
				Teams: map[string]org.Team{
					"duplicate": {},
					"other": {
						Previously: []string{"duplicate"},
					},
				},
			},
			err: true,
		},
		{
			name: "disallow duplicate names (single team)",
			config: org.Config{
				Teams: map[string]org.Team{
					"foo": {
						Previously: []string{"foo"},
					},
				},
			},
			err: true,
		},
		{
			name: "trival case works",
		},
		{
			name: "some of everything",
			config: org.Config{
				Admins:  []string{"keep-admin", "new-admin"},
				Members: []string{"keep-member", "new-member"},
			},
			opt: options{
				maximumDelta: 0.5,
			},
			admins:     []string{"keep-admin", "drop-admin"},
			members:    []string{"keep-member", "drop-member"},
			remove:     []string{"drop-admin", "drop-member"},
			addMembers: []string{"new-member"},
			addAdmins:  []string{"new-admin"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeClient{
				admins:     sets.NewString(tc.admins...),
				members:    sets.NewString(tc.members...),
				removed:    sets.String{},
				newAdmins:  sets.String{},
				newMembers: sets.String{},
			}
			err := configureOrgMembers(tc.opt, fc, fakeOrg, tc.config)
			switch {
			case err != nil:
				if !tc.err {
					t.Errorf("Unexpected error: %v", err)
				}
			case tc.err:
				t.Errorf("Failed to receive error")
			default:
				if err := cmpLists(tc.remove, fc.removed.List()); err != nil {
					t.Errorf("Wrong users removed: %v", err)
				} else if err := cmpLists(tc.addMembers, fc.newMembers.List()); err != nil {
					t.Errorf("Wrong members added: %v", err)
				} else if err := cmpLists(tc.addAdmins, fc.newAdmins.List()); err != nil {
					t.Errorf("Wrong admins added: %v", err)
				}
			}
		})
	}
}

type fakeTeamClient struct {
	teams map[int]github.Team
	max   int
}

func makeFakeTeamClient(teams ...github.Team) *fakeTeamClient {
	fc := fakeTeamClient{
		teams: map[int]github.Team{},
	}
	for _, t := range teams {
		fc.teams[t.ID] = t
		if t.ID >= fc.max {
			fc.max = t.ID + 1
		}
	}
	return &fc
}

const fakeOrg = "random-org"

func (c *fakeTeamClient) CreateTeam(org string, team github.Team) (*github.Team, error) {
	if org != fakeOrg {
		return nil, fmt.Errorf("org must be %s, not %s", fakeOrg, org)
	}
	if team.Name == "fail" {
		return nil, errors.New("injected CreateTeam error")
	}
	c.max++
	team.ID = c.max
	c.teams[team.ID] = team
	return &team, nil

}

func (c *fakeTeamClient) ListTeams(name string) ([]github.Team, error) {
	if name == "fail" {
		return nil, errors.New("injected ListTeams error")
	}
	var teams []github.Team
	for _, t := range c.teams {
		teams = append(teams, t)
	}
	return teams, nil
}

func (c *fakeTeamClient) DeleteTeam(id int) error {
	switch _, ok := c.teams[id]; {
	case !ok:
		return fmt.Errorf("not found %d", id)
	case id < 0:
		return errors.New("injected DeleteTeam error")
	}
	delete(c.teams, id)
	return nil
}

func (c *fakeTeamClient) EditTeam(team github.Team) (*github.Team, error) {
	id := team.ID
	t, ok := c.teams[id]
	if !ok {
		return nil, fmt.Errorf("team %d does not exist", id)
	}
	switch {
	case team.Description == "fail":
		return nil, errors.New("injected description failure")
	case team.Name == "fail":
		return nil, errors.New("injected name failure")
	case team.Privacy == "fail":
		return nil, errors.New("injected privacy failure")
	}
	if team.Description != "" {
		t.Description = team.Description
	}
	if team.Name != "" {
		t.Name = team.Name
	}
	if team.Privacy != "" {
		t.Privacy = team.Privacy
	}
	c.teams[id] = t
	return &t, nil
}

func TestFindTeam(t *testing.T) {
	cases := []struct {
		name     string
		teams    map[string]github.Team
		current  string
		previous []string
		expected int
	}{
		{
			name: "will find current team",
			teams: map[string]github.Team{
				"hello": {ID: 17},
			},
			current:  "hello",
			expected: 17,
		},
		{
			name: "team does not exist returns nil",
			teams: map[string]github.Team{
				"unrelated": {ID: 5},
			},
			current: "hypothetical",
		},
		{
			name: "will find previous name",
			teams: map[string]github.Team{
				"deprecated name": {ID: 1},
			},
			current:  "current name",
			previous: []string{"archaic name", "deprecated name"},
			expected: 1,
		},
		{
			name: "prioritize current when previous also exists",
			teams: map[string]github.Team{
				"deprecated": {ID: 1},
				"current":    {ID: 2},
			},
			current:  "current",
			previous: []string{"deprecated"},
			expected: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actual := findTeam(tc.teams, tc.current, tc.previous...)
			switch {
			case actual == nil:
				if tc.expected != 0 {
					t.Errorf("failed to find team %d", tc.expected)
				}
			case tc.expected == 0:
				t.Errorf("unexpected team returned: %v", *actual)
			case actual.ID != tc.expected:
				t.Errorf("team %v != expected ID %d", actual, tc.expected)
			}
		})
	}
}

func TestConfigureTeams(t *testing.T) {
	desc := "so interesting"
	priv := org.Secret
	cases := []struct {
		name            string
		err             bool
		orgNameOverride string
		config          org.Config
		teams           []github.Team
		expected        map[string]github.Team
		deleted         []int
	}{
		{
			name: "do nothing without error",
		},
		{
			name: "reject duplicated team names (different teams)",
			err:  true,
			config: org.Config{
				Teams: map[string]org.Team{
					"hello": {},
					"there": {Previously: []string{"hello"}},
				},
			},
		},
		{
			name: "reject duplicated team names (single team)",
			err:  true,
			config: org.Config{
				Teams: map[string]org.Team{
					"hello": {Previously: []string{"hello"}},
				},
			},
		},
		{
			name:            "fail to list teams",
			orgNameOverride: "fail",
			err:             true,
		},
		{
			name: "fail to create team",
			config: org.Config{
				Teams: map[string]org.Team{
					"fail": {},
				},
			},
			err: true,
		},
		{
			name: "fail to delete team",
			teams: []github.Team{
				{Name: "fail", ID: -55},
			},
			err: true,
		},
		{
			name: "create missing team",
			teams: []github.Team{
				{Name: "old", ID: 1},
			},
			config: org.Config{
				Teams: map[string]org.Team{
					"new": {},
					"old": {},
				},
			},
			expected: map[string]github.Team{
				"old": {Name: "old", ID: 1},
				"new": {Name: "new", ID: 3},
			},
		},
		{
			name: "reuse existing teams",
			teams: []github.Team{
				{Name: "current", ID: 1},
				{Name: "deprecated", ID: 5},
			},
			config: org.Config{
				Teams: map[string]org.Team{
					"current": {},
					"updated": {Previously: []string{"deprecated"}},
				},
			},
			expected: map[string]github.Team{
				"current": {Name: "current", ID: 1},
				"updated": {Name: "deprecated", ID: 5},
			},
		},
		{
			name: "delete unused teams",
			teams: []github.Team{
				{
					Name: "unused",
					ID:   1,
				},
				{
					Name: "used",
					ID:   2,
				},
			},
			config: org.Config{
				Teams: map[string]org.Team{
					"used": {},
				},
			},
			expected: map[string]github.Team{
				"used": {ID: 2, Name: "used"},
			},
			deleted: []int{1},
		},
		{
			name: "create team with metadata",
			config: org.Config{
				Teams: map[string]org.Team{
					"new": {
						TeamMetadata: org.TeamMetadata{
							Description: &desc,
							Privacy:     &priv,
						},
					},
				},
			},
			expected: map[string]github.Team{
				"new": {ID: 1, Name: "new", Description: desc, Privacy: string(priv)},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := makeFakeTeamClient(tc.teams...)
			orgName := tc.orgNameOverride
			if orgName == "" {
				orgName = fakeOrg
			}
			if tc.expected == nil {
				tc.expected = map[string]github.Team{}
			}
			actual, err := configureTeams(fc, orgName, tc.config)
			switch {
			case err != nil:
				if !tc.err {
					t.Errorf("unexpected error: %v", err)
				}
			case tc.err:
				t.Errorf("failed to receive error")
			case !reflect.DeepEqual(actual, tc.expected):
				t.Errorf("%#v != actual %#v", tc.expected, actual)
			}
			for _, id := range tc.deleted {
				if team, ok := fc.teams[id]; ok {
					t.Errorf("%d still present: %#v", id, team)
				}
			}
		})
	}
}

func TestConfigureTeam(t *testing.T) {
	old := "old value"
	cur := "current value"
	fail := "fail"
	pfail := org.Privacy(fail)
	whatev := "whatever"
	secret := org.Secret
	cases := []struct {
		name     string
		err      bool
		teamName string
		config   org.Team
		github   github.Team
		expected github.Team
	}{
		{
			name:     "patch team when name changes",
			teamName: cur,
			config: org.Team{
				Previously: []string{old},
			},
			github: github.Team{
				ID:   1,
				Name: old,
			},
			expected: github.Team{
				ID:   1,
				Name: cur,
			},
		},
		{
			name:     "patch team when description changes",
			teamName: whatev,
			config: org.Team{
				TeamMetadata: org.TeamMetadata{
					Description: &cur,
				},
			},
			github: github.Team{
				ID:          2,
				Name:        whatev,
				Description: old,
			},
			expected: github.Team{
				ID:          2,
				Name:        whatev,
				Description: cur,
			},
		},
		{
			name:     "patch team when privacy changes",
			teamName: whatev,
			config: org.Team{
				TeamMetadata: org.TeamMetadata{
					Privacy: &secret,
				},
			},
			github: github.Team{
				ID:      3,
				Name:    whatev,
				Privacy: string(org.Closed),
			},
			expected: github.Team{
				ID:      3,
				Name:    whatev,
				Privacy: string(secret),
			},
		},
		{
			name:     "do not patch team when values are the same",
			teamName: fail,
			config: org.Team{
				TeamMetadata: org.TeamMetadata{
					Description: &fail,
					Privacy:     &pfail,
				},
			},
			github: github.Team{
				ID:          4,
				Name:        fail,
				Description: fail,
				Privacy:     fail,
			},
			expected: github.Team{
				ID:          4,
				Name:        fail,
				Description: fail,
				Privacy:     fail,
			},
		},
		{
			name:     "fail to patch team",
			teamName: "team",
			config: org.Team{
				TeamMetadata: org.TeamMetadata{
					Description: &fail,
				},
			},
			github: github.Team{
				ID:          1,
				Name:        "team",
				Description: whatev,
			},
			err: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := makeFakeTeamClient(tc.github)
			err := configureTeam(fc, fakeOrg, tc.teamName, tc.config, tc.github)
			switch {
			case err != nil:
				if !tc.err {
					t.Errorf("unexpected error: %v", err)
				}
			case tc.err:
				t.Errorf("failed to receive expected error")
			case !reflect.DeepEqual(fc.teams[tc.expected.ID], tc.expected):
				t.Errorf("actual %v != expected %v", fc.teams[tc.expected.ID], tc.expected)
			}
		})
	}
}

func TestConfigureTeamMembers(t *testing.T) {
	cases := []struct {
		name           string
		err            bool
		members        sets.String
		maintainers    sets.String
		remove         sets.String
		addMembers     sets.String
		addMaintainers sets.String
		team           org.Team
		id             int
	}{
		{
			name: "fail when listing fails",
			id:   teamID ^ 0xff,
			err:  true,
		},
		{
			name:    "fail when removal fails",
			members: sets.NewString("fail"),
			err:     true,
		},
		{
			name: "fail when add fails",
			team: org.Team{
				Maintainers: []string{"fail"},
			},
			err: true,
		},
		{
			name: "some of everything",
			team: org.Team{
				Maintainers: []string{"keep-maintainer", "new-maintainer"},
				Members:     []string{"keep-member", "new-member"},
			},
			maintainers:    sets.NewString("keep-maintainer", "drop-maintainer"),
			members:        sets.NewString("keep-member", "drop-member"),
			remove:         sets.NewString("drop-maintainer", "drop-member"),
			addMembers:     sets.NewString("new-member"),
			addMaintainers: sets.NewString("new-maintainer"),
		},
	}

	for _, tc := range cases {
		if tc.id == 0 {
			tc.id = teamID
		}
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeClient{
				admins:     sets.StringKeySet(tc.maintainers),
				members:    sets.StringKeySet(tc.members),
				removed:    sets.String{},
				newAdmins:  sets.String{},
				newMembers: sets.String{},
			}
			err := configureTeamMembers(fc, tc.id, tc.team)
			switch {
			case err != nil:
				if !tc.err {
					t.Errorf("Unexpected error: %v", err)
				}
			case tc.err:
				t.Errorf("Failed to receive error")
			default:
				if err := cmpLists(tc.remove.List(), fc.removed.List()); err != nil {
					t.Errorf("Wrong users removed: %v", err)
				} else if err := cmpLists(tc.addMembers.List(), fc.newMembers.List()); err != nil {
					t.Errorf("Wrong members added: %v", err)
				} else if err := cmpLists(tc.addMaintainers.List(), fc.newAdmins.List()); err != nil {
					t.Errorf("Wrong admins added: %v", err)
				}
			}

		})
	}
}

func cmpLists(a, b []string) error {
	if a == nil {
		a = []string{}
	}
	if b == nil {
		b = []string{}
	}
	sort.Strings(a)
	sort.Strings(b)
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("%v != %v", a, b)
	}
	return nil
}
