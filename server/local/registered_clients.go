package local

import (
	"fmt"
	"net/url"
	"os"
	"time"

	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"sourcegraph.com/sourcegraph/go-sourcegraph/sourcegraph"
	"sourcegraph.com/sqs/pbtypes"
	authpkg "src.sourcegraph.com/sourcegraph/auth"
	"src.sourcegraph.com/sourcegraph/auth/idkey"
	"src.sourcegraph.com/sourcegraph/fed"
	"src.sourcegraph.com/sourcegraph/pkg/oauth2util"
	"src.sourcegraph.com/sourcegraph/server/accesscontrol"
	"src.sourcegraph.com/sourcegraph/store"
	"src.sourcegraph.com/sourcegraph/util/metricutil"
	"src.sourcegraph.com/sourcegraph/util/randstring"
)

var RegisteredClients sourcegraph.RegisteredClientsServer = &registeredClients{}

type registeredClients struct{}

var _ sourcegraph.RegisteredClientsServer = (*registeredClients)(nil)

func (s *registeredClients) Get(ctx context.Context, client *sourcegraph.RegisteredClientSpec) (*sourcegraph.RegisteredClient, error) {
	// Catch a common programming mistake: trying to look up a host's
	// client ID in its own list of registered clients. It doesn't
	// make any sense for a host to register with itself as a
	// client. This means there's a bug somewhere upstream.
	if k := idkey.FromContext(ctx); k.ID == client.ID {
		msg := fmt.Sprintf("attempted to look up this server's own client ID %s (almost certainly indicates the presence of a bug)", k.ID)
		if os.Getenv("DEBUG") == "" {
			return nil, grpc.Errorf(codes.InvalidArgument, "%s", msg)
		}
		panic(msg)
	}

	store, err := registeredClientsOrError(ctx)
	if err != nil {
		return nil, err
	}
	return store.Get(ctx, *client)
}

func (s *registeredClients) GetCurrent(ctx context.Context, _ *pbtypes.Void) (*sourcegraph.RegisteredClient, error) {
	actor := authpkg.ActorFromContext(ctx)
	if actor.ClientID == "" {
		return nil, grpc.Errorf(codes.NotFound, "RegisteredClients.GetCurrent: no authenticated registered API client")
	}
	if actor.ClientID == idkey.FromContext(ctx).ID {
		return nil, grpc.Errorf(codes.NotFound, "RegisteredClients.GetCurrent: current credentials represent the server itself, which is not a registered client of itself")
	}

	store, err := registeredClientsOrError(ctx)
	if err != nil {
		return nil, err
	}
	return store.Get(ctx, sourcegraph.RegisteredClientSpec{ID: actor.ClientID})
}

func (s *registeredClients) Create(ctx context.Context, client *sourcegraph.RegisteredClient) (*sourcegraph.RegisteredClient, error) {
	clientStore, err := registeredClientsOrError(ctx)
	if err != nil {
		return nil, err
	}

	if err := checkRedirectURIs(client); err != nil {
		return nil, err
	}

	if client.ClientSecret != "" {
		return nil, grpc.Errorf(codes.InvalidArgument, "client secret must be empty (it is generated by the server)")
	}

	if client.JWKS == "" {
		// Client ID and secret auth.

		if client.ID != "" {
			return nil, grpc.Errorf(codes.InvalidArgument, "client ID must be empty when using client ID/secret credentials (it is generated by the server)")
		}

		// Generate credentials.
		client.ID = randstring.NewLen(30)
		client.ClientSecret = randstring.NewLen(45)
	} else {
		// JWKS public key auth.

		if client.ID == "" {
			return nil, grpc.Errorf(codes.InvalidArgument, "client ID must be set when using JWKS public keys")
		}

		pubKey, err := idkey.UnmarshalJWKSPublicKey([]byte(client.JWKS))
		if err != nil {
			return nil, err
		}
		fp, err := idkey.Fingerprint(pubKey)
		if err != nil {
			return nil, err
		}
		if fp != client.ID {
			return nil, grpc.Errorf(codes.InvalidArgument, "client ID must be set to public key fingerprint")
		}
	}

	if client.ID == idkey.FromContext(ctx).ID {
		return nil, grpc.Errorf(codes.InvalidArgument, "can't register this server as its own client (ClientID == %q)", client.ID)
	}

	if client.ClientSecret != "" && client.JWKS != "" {
		return nil, grpc.Errorf(codes.InvalidArgument, "client ID/secret and JWKS public key auth schemes are mutually exclusive")
	}

	client.CreatedAt = pbtypes.NewTimestamp(time.Now())
	client.UpdatedAt = client.CreatedAt

	if err := clientStore.Create(ctx, *client); err != nil {
		return nil, err
	}

	// if a UID is specified, set that user as an admin on the client
	actorUID := authpkg.ActorFromContext(ctx).UID
	if actorUID != 0 {
		adminOpt := &sourcegraph.UserPermissions{
			UID:      int32(actorUID),
			ClientID: client.ID,
			Read:     true,
			Write:    true,
			Admin:    true,
		}

		if _, err := setPermissionsForUser(ctx, adminOpt); err != nil {
			// if user permissions store is unavailable on the server, ignore and continue.
			if _, ok := err.(*sourcegraph.NotImplementedError); !ok {
				return nil, err
			}
		}
	}

	metricutil.LogEvent(ctx, &sourcegraph.UserEvent{
		Type:     "notif",
		ClientID: client.ID,
		Service:  "RegisteredClients",
		Method:   "Create",
		Result:   "success",
	})

	return client, nil
}

func checkActorAuthedAsClient(ctx context.Context, client sourcegraph.RegisteredClientSpec) error {
	aid := authpkg.ActorFromContext(ctx).ClientID
	if aid != client.ID {
		return grpc.Errorf(codes.PermissionDenied, "ClientID must match (authenticated as %s, target %s)", aid, client.ID)
	}
	return nil
}

func (s *registeredClients) Update(ctx context.Context, client *sourcegraph.RegisteredClient) (*pbtypes.Void, error) {
	if isAdmin, err := s.checkCtxUserIsAdmin(ctx, client.ID); err != nil {
		return nil, err
	} else if !isAdmin {
		return nil, grpc.Errorf(codes.PermissionDenied, "RegisteredClients.Update: need admin access on client to complete this operation")
	}

	store, err := registeredClientsOrError(ctx)
	if err != nil {
		return nil, err
	}

	if err := checkRedirectURIs(client); err != nil {
		return nil, err
	}

	if client.ClientSecret != "" {
		return nil, grpc.Errorf(codes.InvalidArgument, "RegisteredClients.Update of secret is not allowed")
	}

	client.UpdatedAt = pbtypes.NewTimestamp(time.Now())

	if err := store.Update(ctx, *client); err != nil {
		return nil, err
	}
	return &pbtypes.Void{}, nil
}

func (s *registeredClients) Delete(ctx context.Context, client *sourcegraph.RegisteredClientSpec) (*pbtypes.Void, error) {
	if isAdmin, err := s.checkCtxUserIsAdmin(ctx, client.ID); err != nil {
		return nil, err
	} else if !isAdmin {
		return nil, grpc.Errorf(codes.PermissionDenied, "RegisteredClients.Delete: need admin access on client to complete this operation")
	}

	store, err := registeredClientsOrError(ctx)
	if err != nil {
		return nil, err
	}

	if err := store.Delete(ctx, *client); err != nil {
		return nil, err
	}
	return &pbtypes.Void{}, nil
}

func (s *registeredClients) List(ctx context.Context, opt *sourcegraph.RegisteredClientListOptions) (*sourcegraph.RegisteredClientList, error) {
	if err := accesscontrol.VerifyUserHasAdminAccess(ctx, "RegisteredClients.List"); err != nil {
		return nil, err
	}

	store, err := registeredClientsOrError(ctx)
	if err != nil {
		return nil, err
	}

	return store.List(ctx, *opt)
}

func (s *registeredClients) GetUserPermissions(ctx context.Context, opt *sourcegraph.UserPermissionsOptions) (*sourcegraph.UserPermissions, error) {
	if opt.ClientSpec.ID == "" || opt.UID == 0 {
		return nil, grpc.Errorf(codes.InvalidArgument, "RegisteredClients.GetUserPermissions: caller must specify valid clientID and UID")
	}

	userPermsStore, err := userPermissionsOrError(ctx)
	if err != nil {
		return nil, err
	}

	if authpkg.ActorFromContext(ctx).UID != int(opt.UID) {
		// check if user is admin on client.
		if isAdmin, err := s.checkCtxUserIsAdmin(ctx, opt.ClientSpec.ID); err != nil {
			return nil, err
		} else if !isAdmin {
			// check if user is admin on fed root server.
			if err := accesscontrol.VerifyUserHasAdminAccess(ctx, "RegisteredClients.GetUserPermissions"); err != nil {
				return nil, err
			}
		}
	}

	userPerms, err := userPermsStore.Get(ctx, opt)
	if err != nil {
		return nil, err
	}
	return userPerms, nil
}

func (s *registeredClients) SetUserPermissions(ctx context.Context, userPerms *sourcegraph.UserPermissions) (*pbtypes.Void, error) {
	if userPerms.ClientID == "" || userPerms.UID == 0 {
		return nil, grpc.Errorf(codes.InvalidArgument, "RegisteredClients.SetUserPermissions: caller must specify valid clientID and UID")
	}

	if isAdmin, err := s.checkCtxUserIsAdmin(ctx, userPerms.ClientID); err != nil {
		return nil, err
	} else if !isAdmin {
		// check if user is admin on fed root server.
		if err := accesscontrol.VerifyUserHasAdminAccess(ctx, "RegisteredClients.SetUserPermissions"); err != nil {
			return nil, err
		}
	}

	return setPermissionsForUser(ctx, userPerms)
}

func (s *registeredClients) ListUserPermissions(ctx context.Context, client *sourcegraph.RegisteredClientSpec) (*sourcegraph.UserPermissionsList, error) {
	if client.ID == "" {
		return &sourcegraph.UserPermissionsList{}, nil
	}

	userPermsStore, err := userPermissionsOrError(ctx)
	if err != nil {
		return nil, err
	}

	if isAdmin, err := s.checkCtxUserIsAdmin(ctx, client.ID); err != nil {
		return nil, err
	} else if !isAdmin {
		// check if user is admin on fed root server.
		if err := accesscontrol.VerifyUserHasAdminAccess(ctx, "RegisteredClients.ListUserPermissions"); err != nil {
			return nil, err
		}
	}

	userPermsList, err := userPermsStore.List(ctx, client)
	if err != nil {
		return nil, err
	}
	return userPermsList, nil
}

func (s *registeredClients) checkCtxUserIsAdmin(ctx context.Context, clientID string) (bool, error) {
	actor := authpkg.ActorFromContext(ctx)
	if !actor.IsAuthenticated() && actor.ClientID == clientID {
		// If ctx is not authenticated with a user, check if actor has a special scope
		// that grants admin access on that client.
		for _, scope := range actor.Scope {
			// internal server commands have default admin access.
			if scope == "internal:cli" {
				return true, nil
			}
		}
		return false, nil
	}
	userPermsStore, err := userPermissionsOrError(ctx)
	if err != nil {
		return false, err
	}
	return userPermsStore.Verify(ctx, &sourcegraph.UserPermissions{
		UID:      int32(actor.UID),
		ClientID: clientID,
		Admin:    true,
	})
}

// NOTE(security): This function allows the caller to set a user as an admin on a client's
// whitelist without enforcing that the caller itself be authorized by an admin user.
// The only instance where this should be called from is RegisteredClients.Create, to add
// the user registering the client as an admin on the client, since that will be the first admin
// user on that client. After that, only an existing admin should be able to create new admins,
// via the gRPC endpoint RegisteredClients.SetUserPermissions
func setPermissionsForUser(ctx context.Context, userPerms *sourcegraph.UserPermissions) (*pbtypes.Void, error) {
	userPermsStore, err := userPermissionsOrError(ctx)
	if err != nil {
		return nil, err
	}
	if err := userPermsStore.Set(ctx, userPerms); err != nil {
		return nil, err
	}

	metricutil.LogEvent(ctx, &sourcegraph.UserEvent{
		Type:     "notif",
		UID:      userPerms.UID,
		ClientID: userPerms.ClientID,
		Service:  "RegisteredClients",
		Method:   "SetPermissionsForUser",
		Result:   "success",
	})

	return &pbtypes.Void{}, nil
}

func registeredClientsOrError(ctx context.Context) (store.RegisteredClients, error) {
	s := store.RegisteredClientsFromContextOrNil(ctx)
	if s == nil {
		return nil, &sourcegraph.NotImplementedError{What: "RegisteredClients"}
	}
	if !fed.Config.AllowsClientRegistration() {
		return nil, grpc.Errorf(codes.Unimplemented, "server is not a federation root and therefore does not allow client registration")
	}
	return s, nil
}

func userPermissionsOrError(ctx context.Context) (store.UserPermissions, error) {
	s := store.UserPermissionsFromContextOrNil(ctx)
	if s == nil {
		return nil, &sourcegraph.NotImplementedError{What: "UserPermissions"}
	}
	if authpkg.ActorFromContext(ctx).UID == 0 {
		return nil, grpc.Errorf(codes.Unauthenticated, "RegisteredClients.UserPermissions: no authenticated user in context")
	}
	return s, nil
}

func checkRedirectURIs(client *sourcegraph.RegisteredClient) error {
	for _, urlStr := range client.RedirectURIs {
		u, err := url.Parse(urlStr)
		if err != nil {
			return err
		}
		if err := oauth2util.CheckRedirectURI(u); err != nil {
			return err
		}
	}
	return nil
}
