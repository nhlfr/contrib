/*
Copyright 2016 The Kubernetes Authors All rights reserved.

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

package sync

import (
	"fmt"
	"strings"

	"github.com/golang/glog"
	"k8s.io/contrib/mungegithub/github"
	"k8s.io/kubernetes/pkg/util/sets"
)

// IssueFinder finds an issue for a given key.
type IssueFinder interface {
	AllIssuesForKey(key string) []int
	Created(key string, number int)
}

// IssueSource can be implemented by anything that wishes to be synced with
// github issues.
type IssueSource interface {
	// Title is used to identify issues, so you must never change the
	// mechanism or you'll get duplicates.
	Title() string

	// If ID() is found in either the body of the issue or the body of any
	// of its comments, then a new comment doesn't need to be made. A URL
	// to more details is a good choice.
	// Additionally, ID is used to tell if we've already successfully
	// synced a given source. So it must be unique for every source.
	ID() string

	// If a new issue or comment must be made, Body is called to get the
	// text. Body *must* contain the output of ID().
	// newIssue will be true if we are starting a new issue, otherwise we
	// are adding a comment to an existing issue.
	Body(newIssue bool) string

	// If an issue is filed, these labels will be applied.
	Labels() []string
}

// IssueSyncer implements robust issue syncing logic and won't file duplicates etc.
type IssueSyncer struct {
	config *github.Config
	finder IssueFinder
	synced sets.String
}

// NewIssueSyncer constructs an issue syncer.
func NewIssueSyncer(config *github.Config, finder IssueFinder) *IssueSyncer {
	return &IssueSyncer{
		config: config,
		finder: finder,
		synced: sets.NewString(),
	}
}

// Sync syncs the issue. It is fine and cheap to call Sync repeatedly for the
// same source.
func (s *IssueSyncer) Sync(source IssueSource) error {
	if s.synced.Has(source.ID()) {
		return nil
	}

	found, updatableIssues, err := s.findPreviousIssues(source)
	if err != nil {
		return err
	}

	// Close dups if there are multiple open issues
	if len(updatableIssues) > 1 {
		obj := updatableIssues[0]
		if err := s.markAsDups(updatableIssues[1:], *obj.Issue.Number); err != nil {
			return err
		}
	}

	if found {
		// Don't need to update, we were only here to close the dups.
		s.synced.Insert(source.ID())
		return nil
	}

	// Update an issue if possible.
	if len(updatableIssues) > 0 {
		obj := updatableIssues[0]
		// Update the chosen issue
		if err := s.updateIssue(obj, source); err != nil {
			return fmt.Errorf("error updating issue %v for %v: %v", *obj.Issue.Number, source.ID(), err)
		}
		s.synced.Insert(source.ID())
		return nil
	}

	// No issue could be updated, create a new issue.
	n, err := s.createIssue(source)
	if err != nil {
		return fmt.Errorf("error making issue for %v: %v", source.ID, err)
	}
	s.finder.Created(source.Title(), n)
	s.synced.Insert(source.ID())
	return nil
}

// Look through all issues filed about this item.
// If foundIn is > 0, then the particular item was found in that issue.
// All open issues for this item are returned in updatableIssues.
func (s *IssueSyncer) findPreviousIssues(source IssueSource) (found bool, updatableIssues []*github.MungeObject, err error) {
	possibleIssues := s.finder.AllIssuesForKey(source.Title())
	for _, previousIssue := range possibleIssues {
		obj, err := s.config.GetObject(previousIssue)
		if err != nil {
			return false, nil, fmt.Errorf("error getting object for %v: %v", previousIssue, err)
		}
		isRecorded, err := s.isRecorded(obj, source)
		if err != nil {
			return false, nil, fmt.Errorf("error checking whether item %v is recorded in issue %v: %v", source.ID(), previousIssue, err)
		}
		if isRecorded {
			found = true
			// keep going since we may want to close dups
		}
		if obj.Issue.State != nil && *obj.Issue.State == "open" {
			updatableIssues = append(updatableIssues, obj)
		}
	}
	return found, updatableIssues, nil
}

// Close all of the dups.
func (s *IssueSyncer) markAsDups(dups []*github.MungeObject, of int) error {
	// Somehow we got duplicate issues all open at once.
	// Close all of the older ones.
	for _, dup := range dups {
		if err := dup.CloseIssuef("This is a duplicate of #%v; closing", of); err != nil {
			return fmt.Errorf("failed to close %v as a dup of %v: %v", *dup.Issue.Number, of, err)
		}
	}
	return nil
}

// Search through the body and comments to see if the given item is already
// mentioned in the given github issue.
func (s *IssueSyncer) isRecorded(obj *github.MungeObject, source IssueSource) (bool, error) {
	id := source.ID()
	if obj.Issue.Body != nil && strings.Contains(*obj.Issue.Body, id) {
		// We already wrote this item
		return true, nil
	}
	comments, err := obj.ListComments()
	if err != nil {
		return false, fmt.Errorf("error getting comments for %v: %v", *obj.Issue.Number, err)
	}
	for _, c := range comments {
		if c.Body == nil {
			continue
		}
		if strings.Contains(*c.Body, id) {
			// We already wrote this item
			return true, nil
		}
	}
	return false, nil
}

// updateIssue adds a comment about the item to the github object.
func (s *IssueSyncer) updateIssue(obj *github.MungeObject, source IssueSource) error {
	body := source.Body(false)
	id := source.ID()
	if !strings.Contains(body, source.ID()) {
		// prevent making tons of duplicate comments
		panic(fmt.Errorf("Programmer error: %v does not contain %v!", body, id))
	}
	glog.Infof("Updating issue %v with item %v", *obj.Issue.Number, source.ID())
	return obj.WriteComment(body)
}

// createIssue makes a new issue for the given item. If we know about other
// issues for the item, then they'll be referenced.
func (s *IssueSyncer) createIssue(source IssueSource) (issueNumber int, err error) {
	body := source.Body(true)
	id := source.ID()
	if !strings.Contains(body, source.ID()) {
		// prevent making tons of duplicate comments
		panic(fmt.Errorf("Programmer error: %v does not contain %v!", body, id))
	}

	obj, err := s.config.NewIssue(
		source.Title(),
		body,
		source.Labels(),
	)
	if err != nil {
		return 0, err
	}
	glog.Infof("Created issue %v:\n%v", *obj.Issue.Number, body)
	return *obj.Issue.Number, nil
}
