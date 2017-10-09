// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License.  You may obtain a copy
// of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.  See the
// License for the specific language governing permissions and limitations
// under the License.

package utils

import (
	"fmt"
	"os/user"
	"strconv"
	"syscall"
)

// UnixUser represents a Unix user account.  This differs from the standard user.User in that the
// values of the fields here are in integer form as is customary in Unix.
type UnixUser struct {
	// Username is the login name.
	Username string
	// UID is the numeric user identifier.
	UID int
	// GID is the numeric group identifier.
	GID int
	// Groups is the list of secondary groups this user belongs to.
	Groups []int
}

// String formats the user details for display.
func (u *UnixUser) String() string {
	return fmt.Sprintf("username=%s, uid=%d, gid=%d, groups=%v", u.Username, u.UID, u.GID, u.Groups)
}

// ToCredential converts the user details to a credential usable by exec.Cmd.
func (u *UnixUser) ToCredential() *syscall.Credential {
	groups := make([]uint32, len(u.Groups))
	for i, gid := range u.Groups {
		groups[i] = uint32(gid)
	}
	return &syscall.Credential{
		Uid:    uint32(u.UID),
		Gid:    uint32(u.GID),
		Groups: groups,
	}
}

// toUnixUser converts a generic user.User object into a Unix-specific user.
func toUnixUser(user *user.User) (*UnixUser, error) {
	uid, err := strconv.Atoi(user.Uid)
	if err != nil {
		return nil, fmt.Errorf("invalid uid %s for user %s: %v", user.Uid, user.Username, err)
	}

	gid, err := strconv.Atoi(user.Gid)
	if err != nil {
		return nil, fmt.Errorf("invalid gid %s for user %s: %v", user.Gid, user.Username, err)
	}

	strGroups, err := user.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("cannot get groups for %s: %v", user.Username, err)
	}
	groups := make([]int, len(strGroups))
	for i, name := range strGroups {
		groups[i], err = strconv.Atoi(name)
		if err != nil {
			return nil, fmt.Errorf("invalid secondary gid %s for user %s: %v", name, user.Username, err)
		}
	}

	return &UnixUser{
		Username: user.Username,
		UID:      uid,
		GID:      gid,
		Groups:   groups,
	}, nil
}

// LookupUser looks up a user by username.
func LookupUser(name string) (*UnixUser, error) {
	generic, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("cannot find user %s: %v", name, err)
	}
	return toUnixUser(generic)
}

// LookupUID looks up a user by UID.
func LookupUID(uid int) (*UnixUser, error) {
	generic, err := user.LookupId(fmt.Sprintf("%d", uid))
	if err != nil {
		return nil, fmt.Errorf("cannot find user %d: %v", uid, err)
	}
	return toUnixUser(generic)
}
