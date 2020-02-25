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
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
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

// WriteErrorForUnwritableNode returns the expected error for operations that cannot succeed on
// unwritable nodes.
//
// Unwritable nodes have read-only permissions.  Because we use default_permissions when mounting
// the daemon, the kernel performs access checks on its own based on those.  For unprivileged users,
// the kernel denies access and returns EACCES without even calling the FUSE handler for the desired
// write operation.  However, when running as root, the kernel bypasses this check and ends up
// calling the operation, which then fails with EPERM.  This function computes the expected error
// code based on this logic.
func WriteErrorForUnwritableNode() error {
	if os.Getuid() == 0 {
		return unix.EPERM
	}
	return unix.EACCES
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

// LookupUserOtherThan searches for a user whose username is different than all given ones.
func LookupUserOtherThan(username ...string) (*UnixUser, error) {
	// Testing a bunch of low-numbered UIDs should be sufficient because most Unix systems,
	// if not all, have system accounts immediately after 0.
	var other *UnixUser
	for i := 1; i < 100; i++ {
		var err error
		other, err = LookupUID(i)
		if err != nil {
			continue
		}

		for _, name := range username {
			if other.Username == name {
				continue
			}
		}
		break
	}
	if other == nil {
		return nil, fmt.Errorf("cannot find an unprivileged user other than %v", username)
	}
	return other, nil
}

// SetCredential updates the spawn attributes of the given command to execute such command under the
// credentials of the given user.
//
// For the simplicity of the caller, the attributes are not modified if the given user is nil or if
// the given user matches the current user.
func SetCredential(cmd *exec.Cmd, user *UnixUser) {
	if user == nil || user.UID == os.Getuid() {
		return
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	if cmd.SysProcAttr.Credential != nil {
		// This function is intended to be used exclusively from tests, and as such we
		// expect the given cmd object to not have credentials set.  If that were the case,
		// it'd indicate a bug in the code that must be fixed: there is no point in
		// returning this as an error.
		panic("SetCredential invoked on a cmd object that already includes user credentials")
	}
	cmd.SysProcAttr.Credential = user.ToCredential()
	cmd.SysProcAttr.Credential.NoSetGroups = true
}
