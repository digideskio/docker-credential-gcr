// Copyright 2016 Google, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/*
Package credhelper implements a Docker credential helper with special facilities
for GCR authentication.
*/
package credhelper

import (
	"bytes"
	"fmt"
	"net/url"
	"os/exec"
	"strings"

	"github.com/GoogleCloudPlatform/docker-credential-gcr/config"
	"github.com/GoogleCloudPlatform/docker-credential-gcr/store"
	"github.com/docker/docker-credential-helpers/credentials"

	"golang.org/x/oauth2/google"
)

const gcrOAuth2Username = "oauth2accesstoken"

// gcrCredHelper implements a credentials.Helper interface backed by a GCR
// credential store.
type gcrCredHelper struct {
	store        store.GCRCredStore
	tokenSources []string

	// helper methods, package exposed for testing
	envToken       func() (string, error)
	gcloudSDKToken func() (string, error)
	credStoreToken func(store.GCRCredStore) (string, error)
}

// NewGCRCredentialHelper returns a Docker credential helper which
// specializes in GCR's authentication schemes.
func NewGCRCredentialHelper(store store.GCRCredStore, userCfg config.UserConfig) credentials.Helper {
	tokenSources := userCfg.TokenSources()
	return &gcrCredHelper{
		store:          store,
		tokenSources:   tokenSources,
		credStoreToken: tokenFromPrivateStore,
		gcloudSDKToken: tokenFromGcloudSDK,
		envToken:       tokenFromEnv,
	}
}

// Delete lists all stored credentials and associated usernames.
func (ch *gcrCredHelper) List() (map[string]string, error) {
	all3pCreds, err := ch.store.AllThirdPartyCreds()
	if err != nil {
		return nil, helperErr("could not retrieve 3p credentials", err)
	}

	resp := make(map[string]string)

	for registry, creds := range all3pCreds {
		resp[registry] = creds.Username
	}

	for gcrRegistry := range config.SupportedGCRRegistries {
		resp[gcrRegistry] = gcrOAuth2Username
	}

	return resp, nil
}

// Add adds new third-party credentials to the keychain.
func (ch *gcrCredHelper) Add(creds *credentials.Credentials) error {
	serverURL := creds.ServerURL
	if isAGCRHostname(serverURL) {
		errStr := "this operation is unsupported for GCR, please see docker-credential-gcr documentation for supported login methods. " +
			"'gcloud docker' is unnecessary when using docker-credential-gcr, use 'docker' instead."
		return helperErr(errStr, nil)
	}
	if err := ch.store.SetOtherCreds(creds); err != nil {
		return helperErr("could not store 3p credentials for "+serverURL, err)
	}
	return nil
}

// Delete removes third-party credentials from the store.
func (ch *gcrCredHelper) Delete(serverURL string) error {
	if isAGCRHostname(serverURL) {
		return helperErr("delete is unimplemented for GCR: "+serverURL, nil)
	}
	if err := ch.store.DeleteOtherCreds(serverURL); err != nil {
		return helperErr("could not delete 3p credentials for "+serverURL, err)
	}
	return nil
}

// Get returns the username and secret to use for a given registry server URL.
func (ch *gcrCredHelper) Get(serverURL string) (string, string, error) {
	if isAGCRHostname(serverURL) {
		// Return GCR's access token.
		accessToken, err := ch.getGCRAccessToken()
		if err != nil {
			return "", "", helperErr(fmt.Sprintf("could not retrieve %s's access token", serverURL), err)
		}
		return gcrOAuth2Username, accessToken, nil
	}

	// Attempt to retrieve credentials for another repository
	creds, err := ch.store.GetOtherCreds(serverURL)
	if err != nil {
		if credentials.IsErrCredentialsNotFound(err) {
			return "", "", err
		}
		return "", "", helperErr("could not retrieve 3p credentials for "+serverURL, err)
	}
	return creds.Username, creds.Secret, nil
}

// getGCRAccessToken attempts to retrieve a GCR access token from the sources
// listed by ch.tokenSources, in order.
func (ch *gcrCredHelper) getGCRAccessToken() (string, error) {
	var token string
	var err error
	for _, source := range ch.tokenSources {
		switch source {
		case "env":
			token, err = ch.envToken()
		case "gcloud_sdk":
			token, err = ch.gcloudSDKToken()
		case "store":
			token, err = ch.credStoreToken(ch.store)
		default:
			return "", helperErr("unknown token source: "+source, nil)
		}

		// if we successfully retrieved a token, break.
		if err == nil {
			break
		}
	}

	return token, err
}

/*
	tokenFromEnv retrieves a gcloud access_token from the environment.

	From https://godoc.org/golang.org/x/oauth2/google:

	DefaultTokenSource is a token source that uses "Application Default Credentials".

	It looks for credentials in the following places, preferring the first location found:

	1. A JSON file whose path is specified by the
	   GOOGLE_APPLICATION_CREDENTIALS environment variable.
	2. A JSON file in a location known to the gcloud command-line tool.
	   On Windows, this is %APPDATA%/gcloud/application_default_credentials.json.
	   On other systems, $HOME/.config/gcloud/application_default_credentials.json.
	3. On Google App Engine it uses the appengine.AccessToken function.
	4. On Google Compute Engine and Google App Engine Managed VMs, it fetches
	   credentials from the metadata server.
	   (In this final case any provided scopes are ignored.)
*/
func tokenFromEnv() (string, error) {
	ts, err := google.DefaultTokenSource(config.OAuthHTTPContext, config.GCRScopes...)
	if err != nil {
		return "", err
	}

	token, err := ts.Token()
	if err != nil {
		return "", err
	}

	if !token.Valid() {
		return "", helperErr("token was invalid", nil)
	}

	if token.Type() != "Bearer" {
		return "", helperErr(fmt.Sprintf("expected token type \"Bearer\" but got \"%s\"", token.Type()), nil)
	}

	return token.AccessToken, nil
}

// tokenFromGcloudSDK attempts to generate an access_token using the gcloud SDK.
func tokenFromGcloudSDK() (string, error) {
	// shelling out to gcloud is the only currently supported way of
	// obtaining the gcloud access_token
	if _, err := exec.LookPath("gcloud"); err != nil {
		return "", helperErr("gcloud not found on PATH", nil)
	}

	cmd := exec.Command("gcloud", "auth", "print-access-token")

	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", helperErr("gcloud auth print-access-token failed", err)
	}

	token := strings.TrimSpace(out.String())
	if token == "" {
		return "", helperErr("gcloud auth print-access-token returned empty access_token", nil)
	}
	return token, nil
}

func tokenFromPrivateStore(store store.GCRCredStore) (string, error) {
	gcrAuth, err := store.GetGCRAuth()
	if err != nil {
		return "", err
	}
	ts := gcrAuth.TokenSource(config.OAuthHTTPContext)
	tok, err := ts.Token()
	if err != nil {
		return "", err
	}
	if !tok.Valid() {
		return "", helperErr("token was invalid", nil)
	}

	return tok.AccessToken, nil
}

// isAGCRHostname returns true if the given hostname is one of GCR's
func isAGCRHostname(serverURL string) bool {
	URL, err := url.Parse(serverURL)
	if err != nil {
		return false
	}
	return config.SupportedGCRRegistries[URL.Host] || config.SupportedGCRRegistries[serverURL]
}

func helperErr(message string, err error) error {
	if err == nil {
		return fmt.Errorf("docker-credential-gcr/helper: %s", message)
	}
	return fmt.Errorf("docker-credential-gcr/helper: %s: %v", message, err)
}
