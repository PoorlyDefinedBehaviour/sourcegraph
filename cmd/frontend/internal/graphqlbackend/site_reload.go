package graphqlbackend

import (
	"context"
	"errors"
	"time"

	log15 "gopkg.in/inconshreveable/log15.v2"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/actor"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/processrestart"
)

// canReloadSite is whether the current site can be reloaded via the API. Currently
// only goreman-managed sites can be reloaded. Callers must also check if the actor
// is an admin before actually reloading the site.
var canReloadSite = processrestart.CanRestart()

func (r *schemaResolver) ReloadSite(ctx context.Context) (*EmptyResponse, error) {
	// 🚨 SECURITY: Reloading the site is an interruptive action, so only admins
	// may do it.
	if !actor.FromContext(ctx).IsAdmin() {
		return nil, errors.New("must be admin to reload site")
	}

	if !canReloadSite {
		return nil, errors.New("reloading site is not supported")
	}

	const delay = 750 * time.Millisecond
	log15.Warn("Will reload site (from API request)", "actor", actor.FromContext(ctx))
	time.AfterFunc(delay, func() {
		log15.Warn("Reloading site", "actor", actor.FromContext(ctx))
		if err := processrestart.Restart(); err != nil {
			log15.Error("Error reloading site", "err", err)
		}
	})

	return &EmptyResponse{}, nil
}
