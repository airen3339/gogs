// Copyright 2014 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"xorm.io/builder"
	"xorm.io/xorm"

	"gogs.io/gogs/internal/errutil"
	"gogs.io/gogs/internal/repoutil"
	"gogs.io/gogs/internal/userutil"
)

// removeOrgRepo removes all team-repository relations of organization.
func (u *User) removeOrgRepo(e Engine, repoID int64) error {
	return removeOrgRepo(e, u.ID, repoID)
}

// CreateOrganization creates record of a new organization.
func CreateOrganization(org, owner *User) (err error) {
	if err = isUsernameAllowed(org.Name); err != nil {
		return err
	}

	if Users.IsUsernameUsed(context.TODO(), org.Name, 0) {
		return ErrUserAlreadyExist{
			args: errutil.Args{
				"name": org.Name,
			},
		}
	}

	org.LowerName = strings.ToLower(org.Name)
	if org.Rands, err = userutil.RandomSalt(); err != nil {
		return err
	}
	if org.Salt, err = userutil.RandomSalt(); err != nil {
		return err
	}
	org.UseCustomAvatar = true
	org.MaxRepoCreation = -1
	org.NumTeams = 1
	org.NumMembers = 1

	sess := x.NewSession()
	defer sess.Close()
	if err = sess.Begin(); err != nil {
		return err
	}

	if _, err = sess.Insert(org); err != nil {
		return fmt.Errorf("insert organization: %v", err)
	}
	_ = userutil.GenerateRandomAvatar(org.ID, org.Name, org.Email)

	// Add initial creator to organization and owner team.
	if _, err = sess.Insert(&OrgUser{
		UserID:   owner.ID,
		OrgID:    org.ID,
		IsOwner:  true,
		NumTeams: 1,
	}); err != nil {
		return fmt.Errorf("insert org-user relation: %v", err)
	}

	// Create default owner team.
	t := &Team{
		OrgID:      org.ID,
		LowerName:  strings.ToLower(TeamNameOwners),
		Name:       TeamNameOwners,
		Authorize:  AccessModeOwner,
		NumMembers: 1,
	}
	if _, err = sess.Insert(t); err != nil {
		return fmt.Errorf("insert owner team: %v", err)
	}

	if _, err = sess.Insert(&TeamUser{
		UID:    owner.ID,
		OrgID:  org.ID,
		TeamID: t.ID,
	}); err != nil {
		return fmt.Errorf("insert team-user relation: %v", err)
	}

	if err = os.MkdirAll(repoutil.UserPath(org.Name), os.ModePerm); err != nil {
		return fmt.Errorf("create directory: %v", err)
	}

	return sess.Commit()
}

var ErrOrgNotExist = errors.New("Organization does not exist")

// GetOrgByName returns organization by given name.
func GetOrgByName(name string) (*User, error) {
	if name == "" {
		return nil, ErrOrgNotExist
	}
	u := &User{
		LowerName: strings.ToLower(name),
		Type:      UserTypeOrganization,
	}
	has, err := x.Get(u)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrOrgNotExist
	}
	return u, nil
}

// CountOrganizations returns number of organizations.
func CountOrganizations() int64 {
	count, _ := x.Where("type=1").Count(new(User))
	return count
}

// Organizations returns number of organizations in given page.
func Organizations(page, pageSize int) ([]*User, error) {
	orgs := make([]*User, 0, pageSize)
	return orgs, x.Limit(pageSize, (page-1)*pageSize).Where("type=1").Asc("id").Find(&orgs)
}

// deleteBeans deletes all given beans, beans should contain delete conditions.
func deleteBeans(e Engine, beans ...any) (err error) {
	for i := range beans {
		if _, err = e.Delete(beans[i]); err != nil {
			return err
		}
	}
	return nil
}

// DeleteOrganization completely and permanently deletes everything of organization.
func DeleteOrganization(org *User) error {
	err := Users.DeleteByID(context.TODO(), org.ID, false)
	if err != nil {
		return err
	}

	sess := x.NewSession()
	defer sess.Close()
	if err = sess.Begin(); err != nil {
		return err
	}

	if err = deleteBeans(sess,
		&Team{OrgID: org.ID},
		&OrgUser{OrgID: org.ID},
		&TeamUser{OrgID: org.ID},
	); err != nil {
		return fmt.Errorf("deleteBeans: %v", err)
	}
	return sess.Commit()
}

// IsPublicMembership returns true if given user public his/her membership.
func IsPublicMembership(orgId, uid int64) bool {
	has, _ := x.Where("uid=?", uid).And("org_id=?", orgId).And("is_public=?", true).Get(new(OrgUser))
	return has
}

func getOrgsByUserID(sess *xorm.Session, userID int64, showAll bool) ([]*User, error) {
	orgs := make([]*User, 0, 10)
	if !showAll {
		sess.And("`org_user`.is_public=?", true)
	}
	return orgs, sess.And("`org_user`.uid=?", userID).
		Join("INNER", "`org_user`", "`org_user`.org_id=`user`.id").Find(&orgs)
}

// GetOrgsByUserID returns a list of organizations that the given user ID
// has joined.
func GetOrgsByUserID(userID int64, showAll bool) ([]*User, error) {
	return getOrgsByUserID(x.NewSession(), userID, showAll)
}

// getOwnedOrgsByUserID returns a list of organizations are owned by given user ID.
func getOwnedOrgsByUserID(sess *xorm.Session, userID int64) ([]*User, error) {
	orgs := make([]*User, 0, 10)
	return orgs, sess.Where("`org_user`.uid=?", userID).And("`org_user`.is_owner=?", true).
		Join("INNER", "`org_user`", "`org_user`.org_id=`user`.id").Find(&orgs)
}

// GetOwnedOrganizationsByUserIDDesc returns a list of organizations are owned by
// given user ID, ordered descending by the given condition.
func GetOwnedOrgsByUserIDDesc(userID int64, desc string) ([]*User, error) {
	sess := x.NewSession()
	return getOwnedOrgsByUserID(sess.Desc(desc), userID)
}

// getOrgUsersByOrgID returns all organization-user relations by organization ID.
func getOrgUsersByOrgID(e Engine, orgID int64, limit int) ([]*OrgUser, error) {
	orgUsers := make([]*OrgUser, 0, 10)

	sess := e.Where("org_id=?", orgID)
	if limit > 0 {
		sess = sess.Limit(limit)
	}
	return orgUsers, sess.Find(&orgUsers)
}

// ChangeOrgUserStatus changes public or private membership status.
func ChangeOrgUserStatus(orgID, uid int64, public bool) error {
	ou := new(OrgUser)
	has, err := x.Where("uid=?", uid).And("org_id=?", orgID).Get(ou)
	if err != nil {
		return err
	} else if !has {
		return nil
	}

	ou.IsPublic = public
	_, err = x.Id(ou.ID).AllCols().Update(ou)
	return err
}

// removeOrgRepo removes all team-repository relations of given organization.
func removeOrgRepo(e Engine, orgID, repoID int64) error {
	_, err := e.Delete(&TeamRepo{
		OrgID:  orgID,
		RepoID: repoID,
	})
	return err
}

// GetUserMirrorRepositories returns mirror repositories of the organization which the user has access to.
func (u *User) GetUserMirrorRepositories(userID int64) ([]*Repository, error) {
	teamIDs, err := u.GetUserTeamIDs(userID)
	if err != nil {
		return nil, fmt.Errorf("GetUserTeamIDs: %v", err)
	}
	if len(teamIDs) == 0 {
		teamIDs = []int64{-1}
	}

	var teamRepoIDs []int64
	err = x.Table("team_repo").In("team_id", teamIDs).Distinct("repo_id").Find(&teamRepoIDs)
	if err != nil {
		return nil, fmt.Errorf("get team repository ids: %v", err)
	}
	if len(teamRepoIDs) == 0 {
		// team has no repo but "IN ()" is invalid SQL
		teamRepoIDs = []int64{-1} // there is no repo with id=-1
	}

	repos := make([]*Repository, 0, 10)
	if err = x.Where("owner_id = ?", u.ID).
		And("is_private = ?", false).
		Or(builder.In("id", teamRepoIDs)).
		And("is_mirror = ?", true). // Don't move up because it's an independent condition
		Desc("updated_unix").
		Find(&repos); err != nil {
		return nil, fmt.Errorf("get user repositories: %v", err)
	}
	return repos, nil
}
