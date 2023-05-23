package search

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/attribute"

	"github.com/sourcegraph/sourcegraph/enterprise/internal/database"
	"github.com/sourcegraph/sourcegraph/enterprise/internal/own"
	"github.com/sourcegraph/sourcegraph/enterprise/internal/own/codeowners"
	"github.com/sourcegraph/sourcegraph/internal/search"
	"github.com/sourcegraph/sourcegraph/internal/search/job"
	"github.com/sourcegraph/sourcegraph/internal/search/result"
	"github.com/sourcegraph/sourcegraph/internal/search/streaming"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

func NewSelectOwnersJob(child job.Job, features *search.Features) job.Job {
	return &selectOwnersJob{
		child:    child,
		features: features,
	}
}

type selectOwnersJob struct {
	child job.Job

	features *search.Features
}

func (s *selectOwnersJob) Run(ctx context.Context, clients job.RuntimeClients, stream streaming.Sender) (alert *search.Alert, err error) {
	if s.features == nil || !s.features.CodeOwnershipSearch {
		return nil, &featureFlagError{predicate: "select:file.owners"}
	}

	_, ctx, stream, finish := job.StartSpan(ctx, stream, s)
	defer finish(alert, err)

	var (
		mu                    sync.Mutex
		bagMu                 sync.Mutex
		hasResultWithNoOwners bool
		errs                  error
	)

	dedup := result.NewDeduper()
	var maxAlerter search.MaxAlerter

	rules := NewRulesCache(clients.Gitserver, clients.DB)
	bag := own.EmptyBag()

	filteredStream := streaming.StreamFunc(func(event streaming.SearchEvent) {
		var ok bool
		matches, ok, err := getCodeOwnersFromMatches(ctx, &rules, event.Results)
		if err != nil {
			mu.Lock()
			errs = errors.Append(errs, err)
			mu.Unlock()
		}
		mu.Lock()
		if ok {
			hasResultWithNoOwners = true
		}
		func() {
			bagMu.Lock()
			defer bagMu.Unlock()
			for _, m := range matches {
				for _, r := range m.references {
					bag.Add(r)
				}
			}
			bag.Resolve(ctx, database.NewEnterpriseDB(clients.DB))
		}()
		var results result.Matches
		for _, m := range matches {
			for _, r := range m.references {
				ro := bag.Get(r)
				fmt.Printf("owner %s resolves to %s\n", r, ro)
				if ro != nil {
					om := &result.OwnerMatch{
						ResolvedOwner: ownerToResult(ro),
						InputRev:      m.fileMatch.InputRev,
						Repo:          m.fileMatch.Repo,
						CommitID:      m.fileMatch.CommitID,
					}
					if !dedup.Seen(om) {
						dedup.Add(om)
						results = append(results, om)
					}
				}
			}
		}
		event.Results = results
		mu.Unlock()
		stream.Send(event)
	})

	alert, err = s.child.Run(ctx, clients, filteredStream)
	if err != nil {
		errs = errors.Append(errs, err)
	}
	maxAlerter.Add(alert)

	if hasResultWithNoOwners {
		maxAlerter.Add(search.AlertForUnownedResult())
	}

	return maxAlerter.Alert, errs
}

func (s *selectOwnersJob) Name() string {
	return "SelectOwnersSearchJob"
}

func (s *selectOwnersJob) Attributes(_ job.Verbosity) []attribute.KeyValue { return nil }

func (s *selectOwnersJob) Children() []job.Describer {
	return []job.Describer{s.child}
}

func (s *selectOwnersJob) MapChildren(fn job.MapFunc) job.Job {
	cp := *s
	cp.child = job.Map(s.child, fn)
	return &cp
}

type ownerFileMatch struct {
	fileMatch  *result.FileMatch
	references []own.Reference
}

func getCodeOwnersFromMatches(
	ctx context.Context,
	rules *RulesCache,
	matches []result.Match,
) ([]ownerFileMatch, bool, error) {
	var (
		errs                  error
		ownerMatches          []ownerFileMatch
		hasResultWithNoOwners bool
	)

	for _, m := range matches {
		mm, ok := m.(*result.FileMatch)
		if !ok {
			continue
		}
		rs, err := rules.GetFromCacheOrFetch(ctx, mm.Repo.Name, mm.Repo.ID, mm.CommitID)
		if err != nil {
			errs = errors.Append(errs, err)
			continue
		}
		rule := rs.Match(mm.File.Path)
		// No match.
		if !rule.NonEmpty() {
			hasResultWithNoOwners = true
			continue
		}
		refs := rule.References()
		/// LOG LOG LOG
		for _, r := range refs {
			fmt.Printf("for %q found owner %s\n", mm.File.Path, r)
		}
		ownerMatches = append(ownerMatches, ownerFileMatch{
			fileMatch:  mm,
			references: refs,
		})
	}
	return ownerMatches, hasResultWithNoOwners, errs
}

func ownerToResult(o codeowners.ResolvedOwner) result.Owner {
	if v, ok := o.(*codeowners.Person); ok {
		return &result.OwnerPerson{
			Handle: v.Handle,
			Email:  v.GetEmail(),
			User:   v.User,
		}
	}
	if v, ok := o.(*codeowners.Team); ok {
		return &result.OwnerTeam{
			Handle: v.Handle,
			Email:  v.Email,
			Team:   v.Team,
		}
	}
	panic("unimplemented resolved owner in ownerToResult")
}
