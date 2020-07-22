// Copyright (c) 2020, Amazon.com, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package internal

import (
	"context"
	"io/ioutil"
	"net/http"

	"github.com/awslabs/ssosync/internal/aws"
	"github.com/awslabs/ssosync/internal/config"
	"github.com/awslabs/ssosync/internal/google"
	"go.uber.org/zap"

	log "github.com/sirupsen/logrus"
	admin "google.golang.org/api/admin/directory/v1"
)

// SyncGSuite is the interface for synchronising users/groups
type SyncGSuite interface {
	SyncUsers() error
	SyncGroups() error
}

// SyncGSuite is an object type that will synchronise real users and groups
type syncGSuite struct {
	aws    aws.Client
	google google.Client

	users map[string]*aws.User
}

// New will create a new SyncGSuite object
func New(a aws.Client, g google.Client) SyncGSuite {
	return &syncGSuite{
		aws:    a,
		google: g,
		users:  make(map[string]*aws.User),
	}
}

// SyncUsers will Sync Google Users to AWS SSO SCIM
func (s *syncGSuite) SyncUsers() error {
	log.Debug("get deleted users")
	deletedUsers, err := s.google.GetDeletedUsers()
	if err != nil {
		return err
	}

	for _, u := range deletedUsers {
		uu, _ := s.aws.FindUserByEmail(u.PrimaryEmail)
		if uu == nil {
			continue
		}

		log.WithFields(log.Fields{
			"email": u.PrimaryEmail,
		}).Info("deleting google user")

		if err := s.aws.DeleteUser(uu); err != nil {
			return err
		}
	}

	log.Debug("get active google users")
	googleUsers, err := s.google.GetUsers()
	if err != nil {
		return err
	}

	for _, u := range googleUsers {
		ll := log.WithFields(log.Fields{
			"email": u.PrimaryEmail,
		})

		ll.Debug("finding user")
		uu, _ := s.aws.FindUserByEmail(u.PrimaryEmail)
		if uu != nil {
			s.users[uu.Username] = uu
			continue
		}

		ll.Info("creating user")
		uu, err := s.aws.CreateUser(aws.NewUser(
			u.Name.GivenName,
			u.Name.FamilyName,
			u.PrimaryEmail,
		))
		if err != nil {
			return err
		}

		s.users[uu.Username] = uu
	}

	return nil
}

// SyncGroups will sync groups from Google -> AWS SSO
func (s *syncGSuite) SyncGroups() error {
	log.Debug("get sso groups")
	awsGroups, err := s.aws.GetGroups()
	if err != nil {
		return err
	}

	log.Debug("get google groups")
	googleGroups, err := s.google.GetGroups()
	if err != nil {
		return err
	}

	correlatedGroups := make(map[string]*aws.Group)

	for _, g := range googleGroups {
		log := log.WithFields(log.Fields{
			"group": g.Name,
		})

		log.Debug("Check group")

		var group *aws.Group

		if awsGroup, ok := (*awsGroups)[g.Name]; ok {
			log.Debug("Found group")
			correlatedGroups[awsGroup.DisplayName] = &awsGroup
			group = &awsGroup
		} else {
			log.Info("Creating group in AWS")
			newGroup, err := s.aws.CreateGroup(aws.NewGroup(g.Name))
			if err != nil {
				return err
			}
			correlatedGroups[newGroup.DisplayName] = newGroup
			group = newGroup
		}

		groupMembers, err := s.google.GetGroupMembers(g)
		if err != nil {
			return err
		}

		memberList := make(map[string]*admin.Member)

		log.Info("Start group user sync")

		for _, m := range groupMembers {
			if _, ok := s.users[m.Email]; ok {
				memberList[m.Email] = m
			}
		}

		for _, u := range s.users {
			log.WithField("user", u.Username).Debug("Checking user is in group already")
			b, err := s.aws.IsUserInGroup(u, group)
			if err != nil {
				return err
			}

			if _, ok := memberList[u.Username]; ok {
				if !b {
					log.WithField("user", u.Username).Info("Adding user to group")
					err := s.aws.AddUserToGroup(u, group)
					if err != nil {
						return err
					}
				}
			} else {
				if b {
					log.WithField("user", u.Username).Info("Removing user from group")
					err := s.aws.RemoveUserFromGroup(u, group)
					if err != nil {
						return err
					}
				}
			}
		}
	}

	log.Info("Clean up AWS groups")
	for _, g := range *awsGroups {
		if _, ok := correlatedGroups[g.DisplayName]; !ok {
			log.Info("Delete Group in AWS", zap.String("group", g.DisplayName))
			err := s.aws.DeleteGroup(&g)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// DoSync will create a logger and run the sync with the paths
// given to do the sync.
func DoSync(ctx context.Context, cfg *config.Config) error {
	log.Info("Creating the Google and AWS Clients needed")

	creds := []byte(cfg.GoogleCredentials)

	if !cfg.IsLambda {
		b, err := ioutil.ReadFile(cfg.GoogleCredentials)
		if err != nil {
			return err
		}
		creds = b
	}

	googleClient, err := google.NewClient(ctx, cfg.GoogleAdmin, creds)
	if err != nil {
		return err
	}

	awsClient, err := aws.NewClient(
		&http.Client{},
		&aws.Config{
			Endpoint: cfg.SCIMEndpoint,
			Token:    cfg.SCIMAccessToken,
		})
	if err != nil {
		return err
	}

	c := New(awsClient, googleClient)
	err = c.SyncUsers()
	if err != nil {
		return err
	}

	err = c.SyncGroups()
	if err != nil {
		return err
	}

	return nil
}
