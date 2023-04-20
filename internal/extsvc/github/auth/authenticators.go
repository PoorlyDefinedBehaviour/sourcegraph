package auth

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/sourcegraph/sourcegraph/internal/extsvc/auth"
	"github.com/sourcegraph/sourcegraph/internal/extsvc/github"
	"github.com/sourcegraph/sourcegraph/internal/httpcli"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

// gitHubAppAuthenticator is used to authenticate requests to the GitHub API
// using a GitHub App. It contains the ID and private key associated with
// the GitHub App.
type gitHubAppAuthenticator struct {
	appID  string
	key    *rsa.PrivateKey
	rawKey []byte
}

// NewGitHubAppAuthenticator creates an Authenticator that can be used to authenticate requests
// to the GitHub API as a GitHub App. It requires the GitHub App ID and RSA private key.
//
// The returned Authenticator can be used to sign requests to the GitHub API on behalf of the GitHub App.
// The requests will contain a JSON Web Token (JWT) in the Authorization header with claims identifying
// the GitHub App.
func NewGitHubAppAuthenticator(appID string, privateKey []byte) (*gitHubAppAuthenticator, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM(privateKey)
	if err != nil {
		return nil, errors.Wrap(err, "parse private key")
	}
	return &gitHubAppAuthenticator{
		appID:  appID,
		key:    key,
		rawKey: privateKey,
	}, nil
}

// Authenticate adds an Authorization header to the request containing
// a JSON Web Token (JWT) signed with the GitHub App's private key.
// The JWT contains claims identifying the GitHub App.
func (a *gitHubAppAuthenticator) Authenticate(r *http.Request) error {
	token, err := a.generateJWT()
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// generateJWT generates a JSON Web Token (JWT) signed with the GitHub App's private key.
// The JWT contains claims identifying the GitHub App.
//
// The payload computation is following GitHub App's Ruby example shown in
// https://docs.github.com/en/developers/apps/building-github-apps/authenticating-with-github-apps#authenticating-as-a-github-app.
//
// NOTE: GitHub rejects expiry and issue timestamps that are not an integer,
// while the jwt-go library serializes to fractional timestamps. Truncate them
// before passing to jwt-go.
//
// The returned JWT can be used to authenticate requests to the GitHub API as the GitHub App.
func (a *gitHubAppAuthenticator) generateJWT() (string, error) {
	iss := time.Now().Add(-time.Minute).Truncate(time.Second)
	exp := iss.Add(10 * time.Minute)
	claims := &jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(iss),
		ExpiresAt: jwt.NewNumericDate(exp),
		Issuer:    a.appID,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)

	return token.SignedString(a.key)
}

func (a *gitHubAppAuthenticator) Hash() string {
	shaSum := sha256.Sum256(a.rawKey)
	return hex.EncodeToString(shaSum[:])
}

type jsonToken struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

type InstallationAccessToken struct {
	Token     string
	ExpiresAt time.Time
}

// installationAccessToken is used to authenticate requests to the
// GitHub API using an installation access token from a GitHub App.
//
// It implements the auth.Authenticator interface.
type installationAccessToken struct {
	installationID   int
	token            string
	expiresAt        time.Time
	appAuthenticator auth.Authenticator
	cli              *github.V3Client
}

func (t *installationAccessToken) updateFromJSON(jt *jsonToken) {
	expiresAt, err := time.Parse(time.RFC3339, jt.ExpiresAt)
	if err == nil {
		t.expiresAt = expiresAt
	}
	t.token = jt.Token
}

// NewInstallationAccessToken creates an Authenticator that can be used to authenticate requests
// to the GitHub API using an installation access token associated with a GitHub App installation.
//
// The returned Authenticator can be used to authenticate requests to the GitHub API on behalf of the installation.
// The requests will contain the installation access token in the Authorization header.
// When the installation access token expires, the appAuthenticator will be used to generate a new one.
func NewInstallationAccessToken(
	installationID int,
	appAuthenticator auth.Authenticator,
) *installationAccessToken {
	auther := &installationAccessToken{
		installationID:   installationID,
		appAuthenticator: appAuthenticator,
	}
	return auther
}

// Refresh generates a new installation access token for the GitHub App installation.
//
// It makes a request to the GitHub API to generate a new installation access token for the
// installation associated with the Authenticator. It updates the Authenticator with the new
// installation access token and expiry time.
//
// Returns an error if the request fails, or if there is no Authenticator to authenticate
// the token refresh request.
func (t *installationAccessToken) Refresh(ctx context.Context, cli httpcli.Doer) error {
	if t.appAuthenticator == nil {
		return errors.New("appAuthenticator is nil")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", t.installationID), nil)
	if err != nil {
		return err
	}
	t.appAuthenticator.Authenticate(req)

	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var jsonToken jsonToken
	if err := json.NewDecoder(resp.Body).Decode(&jsonToken); err != nil {
		return err
	}
	t.updateFromJSON(&jsonToken)
	return nil
}

// Authenticate adds an Authorization header to the request containing
// the installation access token associated with the GitHub App installation.
func (a *installationAccessToken) Authenticate(r *http.Request) error {
	r.Header.Set("Authorization", "Bearer "+a.token)
	return nil
}

// Hash returns a hash of the GitHub App installation ID.
// We use the installation ID instead of the installation access
// token because installation access tokens are short lived.
func (a *installationAccessToken) Hash() string {
	sum := sha256.Sum256([]byte(strconv.Itoa(a.installationID)))
	return hex.EncodeToString(sum[:])
}

// NeedsRefresh checks whether the GitHub App installation access token
// needs to be refreshed. An access token needs to be refreshed if it has
// expired or will expire within the next few seconds.
func (t *installationAccessToken) NeedsRefresh() bool {
	if t.token == "" {
		return true
	}
	if t.expiresAt.IsZero() {
		return false
	}
	return time.Until(t.expiresAt) < 5*time.Minute
}
