// Package workloadidentity provides GCP credentials for workloads running in
// a cluster set up with identityctl.
//
// Credentials are resolved in order:
//
//  1. If GOOGLE_APPLICATION_CREDENTIALS is set, standard application default
//     credentials are used.
//  2. If a projected service account token exists at TokenPath, it is
//     exchanged for GCP credentials via workload identity federation. The
//     token's own audience identifies the workload identity provider, so no
//     other configuration is needed.
//  3. Otherwise, the application default credentials chain is used (which
//     covers e.g. the GCE/GKE metadata server).
package workloadidentity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/google/externalaccount"
)

// TokenDir is the well-known directory where pods mount the projected
// service account token.
const TokenDir = "/var/run/secrets/identityctl"

// TokenPath is the well-known path of the projected service account token.
const TokenPath = TokenDir + "/token"

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

const audiencePrefix = "https://iam.googleapis.com/projects/"

// TokenSource returns an oauth2.TokenSource for GCP, following the
// resolution order described in the package documentation. If no scopes are
// given, the cloud-platform scope is used.
func TokenSource(ctx context.Context, scopes ...string) (oauth2.TokenSource, error) {
	if len(scopes) == 0 {
		scopes = []string{cloudPlatformScope}
	}
	if os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "" {
		return google.DefaultTokenSource(ctx, scopes...)
	}
	if _, err := os.Stat(TokenPath); err == nil {
		return FileTokenSource(ctx, TokenPath, scopes...)
	}
	return google.DefaultTokenSource(ctx, scopes...)
}

// FileTokenSource builds a token source from a projected service account
// token file. The token's audience must be the workload identity provider's
// default allowed audience (https://iam.googleapis.com/projects/...), from
// which the STS exchange is configured. The file is re-read on each token
// refresh, so kubelet rotation is handled transparently.
func FileTokenSource(ctx context.Context, tokenPath string, scopes ...string) (oauth2.TokenSource, error) {
	if len(scopes) == 0 {
		scopes = []string{cloudPlatformScope}
	}
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("reading service account token %q: %w", tokenPath, err)
	}
	audience, err := providerAudience(string(tokenBytes))
	if err != nil {
		return nil, fmt.Errorf("inspecting service account token %q: %w", tokenPath, err)
	}

	config := externalaccount.Config{
		// The STS audience is the provider resource name prefixed with "//".
		Audience:         strings.TrimPrefix(audience, "https:"),
		SubjectTokenType: "urn:ietf:params:oauth:token-type:jwt",
		TokenURL:         "https://sts.googleapis.com/v1/token",
		CredentialSource: &externalaccount.CredentialSource{
			File:   tokenPath,
			Format: externalaccount.Format{Type: "text"},
		},
		Scopes: scopes,
	}
	tokenSource, err := externalaccount.NewTokenSource(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("building workload identity token source: %w", err)
	}
	return tokenSource, nil
}

// providerAudience extracts the workload identity provider audience from the
// token's aud claim. The token is our own credential, so the claims are read
// without signature verification.
func providerAudience(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decoding token payload: %w", err)
	}
	claims := struct {
		Audience audiences `json:"aud"`
	}{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parsing token claims: %w", err)
	}
	for _, audience := range claims.Audience {
		if strings.HasPrefix(audience, audiencePrefix) && strings.Contains(audience, "/workloadIdentityPools/") {
			return audience, nil
		}
	}
	return "", fmt.Errorf("token audience %v does not identify a workload identity provider (want %s...)", claims.Audience, audiencePrefix)
}

// audiences unmarshals a JWT aud claim, which may be a string or an array.
type audiences []string

func (a *audiences) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*a = audiences{single}
		return nil
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	*a = list
	return nil
}
