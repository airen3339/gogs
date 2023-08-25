// Copyright 2022 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"gorm.io/gorm"

	"gogs.io/gogs/internal/dbutil"
	"gogs.io/gogs/internal/errutil"
)

// OrgsStore is the persistent interface for organizations.
type OrgsStore interface {
	// AddMember adds a new member to the given organization.
	AddMember(ctx context.Context, orgID, userID int64) error
	// RemoveMember removes a member from the given organization.
	RemoveMember(ctx context.Context, orgID, userID int64) error

	// IsOwnedBy returns true if the given user is an owner of the organization.
	IsOwnedBy(ctx context.Context, orgID, userID int64) bool
	// HasMember returns true if the given user is a member of the organization.
	HasMember(ctx context.Context, orgID, userID int64) bool
	// ListMembers returns all members of the given organization, and sorted by the
	// given order (e.g. "id ASC").
	ListMembers(ctx context.Context, orgID int64, opts ListOrgMembersOptions) ([]*User, error)

	// SearchByName returns a list of organizations whose username or full name
	// matches the given keyword case-insensitively. Results are paginated by given
	// page and page size, and sorted by the given order (e.g. "id DESC"). A total
	// count of all results is also returned. If the order is not given, it's up to
	// the database to decide.
	SearchByName(ctx context.Context, keyword string, page, pageSize int, orderBy string) ([]*Organization, int64, error)
	// List returns a list of organizations filtered by options.
	List(ctx context.Context, opts ListOrgsOptions) ([]*Organization, error)
	// CountByUser returns the number of organizations the user is a member of.
	CountByUser(ctx context.Context, userID int64) (int64, error)

	// GetTeamByName returns the team with given name under the given organization.
	// It returns ErrTeamNotExist whe not found.
	GetTeamByName(ctx context.Context, orgID int64, name string) (*Team, error)

	// AccessibleRepositoriesByUser returns a range of repositories in the
	// organization that the user has access to and the total number of it. Results
	// are paginated by given page and page size, and sorted by the given order
	// (e.g. "updated_unix DESC").
	AccessibleRepositoriesByUser(ctx context.Context, orgID, userID int64, page, pageSize int, opts AccessibleRepositoriesByUserOptions) ([]*Repository, int64, error)
}

var Orgs OrgsStore

var _ OrgsStore = (*orgs)(nil)

type orgs struct {
	*gorm.DB
}

// NewOrgsStore returns a persistent interface for orgs with given database
// connection.
func NewOrgsStore(db *gorm.DB) OrgsStore {
	return &orgs{DB: db}
}

func (*orgs) recountMembers(tx *gorm.DB, orgID int64) error {
	/*
		Equivalent SQL for PostgreSQL:

		UPDATE "user"
		SET num_members = (
			SELECT COUNT(*) FROM org_user WHERE org_id = @orgID
		)
		WHERE id = @orgID
	*/
	err := tx.Model(&User{}).
		Where("id = ?", orgID).
		Update(
			"num_members",
			tx.Model(&OrgUser{}).Select("COUNT(*)").Where("org_id = ?", orgID),
		).
		Error
	if err != nil {
		return errors.Wrap(err, `update "user.num_members"`)
	}
	return nil
}

func (db *orgs) AddMember(ctx context.Context, orgID, userID int64) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ou := &OrgUser{
			UserID: userID,
			OrgID:  orgID,
		}
		result := tx.FirstOrCreate(ou, ou)
		if result.Error != nil {
			return errors.Wrap(result.Error, "upsert")
		} else if result.RowsAffected <= 0 {
			return nil // Relation already exists
		}
		return db.recountMembers(tx, orgID)
	})
}

type ErrLastOrgOwner struct {
	args map[string]any
}

func IsErrLastOrgOwner(err error) bool {
	return errors.As(err, &ErrLastOrgOwner{})
}

func (err ErrLastOrgOwner) Error() string {
	return fmt.Sprintf("user is the last owner of the organization: %v", err.args)
}

func (db *orgs) RemoveMember(ctx context.Context, orgID, userID int64) error {
	ou, err := db.getOrgUser(ctx, orgID, userID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil // Not a member
		}
		return errors.Wrap(err, "check organization membership")
	}

	// Check if the member to remove is the last owner.
	if ou.IsOwner {
		t, err := db.GetTeamByName(ctx, orgID, TeamNameOwners)
		if err != nil {
			return errors.Wrap(err, "get owners team")
		} else if t.NumMembers == 1 {
			return ErrLastOrgOwner{args: map[string]any{"orgID": orgID, "userID": userID}}
		}
	}

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		repoIDsConds := db.accessibleRepositoriesByUser(tx, orgID, userID, accessibleRepositoriesByUserOptions{}).Select("repository.id")

		err := tx.Where("user_id = ? AND repo_id IN (?)", userID, repoIDsConds).Delete(&Watch{}).Error
		if err != nil {
			return errors.Wrap(err, "unwatch repositories")
		}

		err = tx.Table("repository").
			UpdateColumn("num_watches", gorm.Expr("num_watches - 1")).
			Where("id IN (?)", repoIDsConds).Error
		if err != nil {
			return errors.Wrap(err, `decrease "repository.num_watches"`)
		}

		err = tx.Where("user_id = ? AND repo_id IN (?)", userID, repoIDsConds).Delete(&Access{}).Error
		if err != nil {
			return errors.Wrap(err, "delete repository accesses")
		}

		// todo: delete team memberships
		// // Delete member in his/her teams.
		// teams, err := getUserTeams(sess, org.ID, user.ID)
		// if err != nil {
		// 	return err
		// }
		// for _, t := range teams {
		// 	if err = removeTeamMember(sess, org.ID, t.ID, user.ID); err != nil {
		// 		return err
		// 	}
		// }

		err = tx.Where("uid = ? AND org_id = ?", userID, orgID).Delete(&OrgUser{}).Error
		if err != nil {
			return errors.Wrap(err, "delete organization membership")
		}
		return db.recountMembers(tx, orgID)
	})
}

type accessibleRepositoriesByUserOptions struct {
	orderBy  string
	page     int
	pageSize int
}

func (*orgs) accessibleRepositoriesByUser(tx *gorm.DB, orgID, userID int64, opts accessibleRepositoriesByUserOptions) *gorm.DB {
	/*
		Equivalent SQL for PostgreSQL:

		<SELECT * FROM "repository">
		JOIN team_repo ON repository.id = team_repo.repo_id
		WHERE
			owner_id = @orgID
		AND (
				team_repo.team_id IN (
					SELECT team_id FROM "team_user"
					WHERE team_user.org_id = @orgID AND uid = @userID)
				)
			OR  (repository.is_private = FALSE AND repository.is_unlisted = FALSE)
		)
		[ORDER BY updated_unix DESC]
		[LIMIT @limit OFFSET @offset]
	*/
	conds := tx.
		Joins("JOIN team_repo ON repository.id = team_repo.repo_id").
		Where("owner_id = ? AND (?)",
			orgID,
			tx.Where("team_repo.team_id IN (?)",
				tx.Select("team_id").
					Table("team_user").
					Where("team_user.org_id = ? AND uid = ?", orgID, userID),
			).
				Or("repository.is_private = ? AND repository.is_unlisted = ?", false, false),
		)
	if opts.orderBy != "" {
		conds.Order(opts.orderBy)
	}
	if opts.page > 0 && opts.pageSize > 0 {
		conds.Limit(opts.pageSize).Offset((opts.page - 1) * opts.pageSize)
	}
	return conds
}

type AccessibleRepositoriesByUserOptions struct {
	// Whether to skip counting the total number of repositories.
	SkipCount bool
}

func (db *orgs) AccessibleRepositoriesByUser(ctx context.Context, orgID, userID int64, page, pageSize int, opts AccessibleRepositoriesByUserOptions) ([]*Repository, int64, error) {
	conds := db.accessibleRepositoriesByUser(
		db.DB,
		orgID,
		userID,
		accessibleRepositoriesByUserOptions{
			orderBy:  "updated_unix DESC",
			page:     page,
			pageSize: pageSize,
		},
	).WithContext(ctx)

	repos := make([]*Repository, 0, pageSize)
	err := conds.Find(&repos).Error
	if err != nil {
		return nil, 0, errors.Wrap(err, "list repositories")
	}

	if opts.SkipCount {
		return repos, 0, nil
	}
	var count int64
	err = conds.Model(&Repository{}).Count(&count).Error
	if err != nil {
		return nil, 0, errors.Wrap(err, "count repositories")
	}
	return repos, count, nil
}

func (db *orgs) getOrgUser(ctx context.Context, orgID, userID int64) (*OrgUser, error) {
	var ou OrgUser
	return &ou, db.WithContext(ctx).Where("org_id = ? AND uid = ?", orgID, userID).First(&ou).Error
}

func (db *orgs) IsOwnedBy(ctx context.Context, orgID, userID int64) bool {
	ou, err := db.getOrgUser(ctx, orgID, userID)
	return err == nil && ou.IsOwner
}

func (db *orgs) HasMember(ctx context.Context, orgID, userID int64) bool {
	_, err := db.getOrgUser(ctx, orgID, userID)
	return err == nil
}

type ListOrgMembersOptions struct {
	// The maximum number of members to return.
	Limit int
}

func (db *orgs) ListMembers(ctx context.Context, orgID int64, opts ListOrgMembersOptions) ([]*User, error) {
	/*
		Equivalent SQL for PostgreSQL:

		SELECT * FROM "user"
		JOIN org_user ON org_user.uid = user.id
		WHERE
			org_user.org_id = @orgID
		ORDER BY user.id ASC
		[LIMIT @limit]
	*/
	conds := db.WithContext(ctx).
		Joins(dbutil.Quote("JOIN org_user ON org_user.uid = %s.id", "user")).
		Where("org_user.org_id = ?", orgID).
		Order(dbutil.Quote("%s.id ASC", "user"))
	if opts.Limit > 0 {
		conds.Limit(opts.Limit)
	}
	var users []*User
	return users, conds.Find(&users).Error
}

type ListOrgsOptions struct {
	// Filter by the membership with the given user ID.
	MemberID int64
	// Whether to include private memberships.
	IncludePrivateMembers bool
}

func (db *orgs) List(ctx context.Context, opts ListOrgsOptions) ([]*Organization, error) {
	if opts.MemberID <= 0 {
		return nil, errors.New("MemberID must be greater than 0")
	}

	/*
		Equivalent SQL for PostgreSQL:

		SELECT * FROM "user"
		JOIN org_user ON org_user.org_id = user.id
		WHERE
			org_user.uid = @memberID
		[AND org_user.is_public = @includePrivateMembers]
		ORDER BY user.id ASC
	*/
	conds := db.WithContext(ctx).
		Joins(dbutil.Quote("JOIN org_user ON org_user.org_id = %s.id", "user")).
		Where("org_user.uid = ?", opts.MemberID).
		Order(dbutil.Quote("%s.id ASC", "user"))
	if !opts.IncludePrivateMembers {
		conds.Where("org_user.is_public = ?", true)
	}

	var orgs []*Organization
	return orgs, conds.Find(&orgs).Error
}

func (db *orgs) SearchByName(ctx context.Context, keyword string, page, pageSize int, orderBy string) ([]*Organization, int64, error) {
	return searchUserByName(ctx, db.DB, UserTypeOrganization, keyword, page, pageSize, orderBy)
}

func (db *orgs) CountByUser(ctx context.Context, userID int64) (int64, error) {
	var count int64
	return count, db.WithContext(ctx).Model(&OrgUser{}).Where("uid = ?", userID).Count(&count).Error
}

var _ errutil.NotFound = (*ErrTeamNotExist)(nil)

type ErrTeamNotExist struct {
	args map[string]any
}

func IsErrTeamNotExist(err error) bool {
	return errors.As(err, &ErrTeamNotExist{})
}

func (err ErrTeamNotExist) Error() string {
	return fmt.Sprintf("team does not exist: %v", err.args)
}

func (ErrTeamNotExist) NotFound() bool {
	return true
}

func (db *orgs) GetTeamByName(ctx context.Context, orgID int64, name string) (*Team, error) {
	var team Team
	err := db.WithContext(ctx).Where("org_id = ? AND lower_name = ?", orgID, strings.ToLower(name)).First(&team).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTeamNotExist{args: map[string]any{"orgID": orgID, "name": name}}
		}
		return nil, errors.Wrap(err, "get team by name")
	}
	return &team, nil
}

type Organization = User

func (u *Organization) TableName() string {
	return "user"
}

// IsOwnedBy returns true if the given user is an owner of the organization.
//
// TODO(unknwon): This is also used in templates, which should be fixed by
// having a dedicated type `template.Organization`.
func (u *Organization) IsOwnedBy(userID int64) bool {
	return Orgs.IsOwnedBy(context.TODO(), u.ID, userID)
}

// OrgUser represents relations of organizations and their members.
type OrgUser struct {
	ID       int64 `gorm:"primaryKey"`
	UserID   int64 `xorm:"uid INDEX UNIQUE(s)" gorm:"column:uid;uniqueIndex:org_user_user_org_unique;index;not null" json:"Uid"`
	OrgID    int64 `xorm:"INDEX UNIQUE(s)" gorm:"uniqueIndex:org_user_user_org_unique;index;not null"`
	IsPublic bool  `gorm:"not null;default:FALSE"`
	IsOwner  bool  `gorm:"not null;default:FALSE"`
	NumTeams int   `gorm:"not null;default:0"`
}
