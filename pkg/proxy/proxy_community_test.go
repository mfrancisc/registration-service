package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/codeready-toolchain/registration-service/pkg/auth"
	"github.com/codeready-toolchain/registration-service/pkg/proxy/handlers"
	"github.com/codeready-toolchain/registration-service/pkg/signup"
	"github.com/codeready-toolchain/registration-service/test/fake"
	commontest "github.com/codeready-toolchain/toolchain-common/pkg/test"
	authsupport "github.com/codeready-toolchain/toolchain-common/pkg/test/auth"
	"go.uber.org/atomic"

	toolchainv1alpha1 "github.com/codeready-toolchain/api/api/v1alpha1"
	testconfig "github.com/codeready-toolchain/toolchain-common/pkg/test/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (s *TestProxySuite) TestProxyCommunityEnabled() {
	// given

	port := "30456"

	env := s.DefaultConfig().Environment()
	defer s.SetConfig(testconfig.RegistrationService().
		Environment(env))
	s.SetConfig(testconfig.RegistrationService().
		Environment(string(testconfig.E2E))) // We use e2e-test environment just to be able to re-use token generation
	_, err := auth.InitializeDefaultTokenParser()
	require.NoError(s.T(), err)

	for _, environment := range []testconfig.EnvName{testconfig.E2E, testconfig.Dev, testconfig.Prod} {
		s.Run("for environment "+string(environment), func() {
			// spin up proxy
			s.SetConfig(
				testconfig.RegistrationService().Environment(string(environment)),
				testconfig.PublicViewerConfig(true),
			)
			proxy, server := s.spinUpProxy(port)
			defer func() {
				_ = server.Close()
			}()

			// wait for proxy to be alive
			s.Run("is alive", func() {
				s.waitForProxyToBeAlive(port)
			})
			s.Run("health check ok", func() {
				s.checkProxyIsHealthy(port)
			})

			// run community tests
			s.checkProxyCommunityOK(proxy, port)
		})
	}
}

func (s *TestProxySuite) checkProxyCommunityOK(proxy *Proxy, port string) {
	podsRequestURL := func(workspace string) string {
		return fmt.Sprintf("http://localhost:%s/workspaces/%s/api/pods", port, workspace)
	}

	podsInNamespaceRequestURL := func(workspace string, namespace string) string {
		return fmt.Sprintf("http://localhost:%s/workspaces/%s/api/namespaces/%s/pods", port, workspace, namespace)
	}

	s.Run("successfully proxy", func() {
		// user with public workspace
		smith := "smith"
		// user with private workspace
		alice := "alice"
		// unsigned user
		bob := "bob"
		// not ready
		john := "john"
		// banned user
		eve, eveEmail := "eve", "eve@somecorp.com"

		// Start the member-2 API Server
		httpTestServerResponse := "my response"
		testServer := httptest.NewServer(nil)
		defer testServer.Close()

		// initialize SignupService
		signupService := fake.NewSignupService(
			&signup.Signup{
				Name:              smith,
				APIEndpoint:       testServer.URL,
				ClusterName:       "member-2",
				CompliantUsername: smith,
				Username:          "smith@",
				Status: signup.Status{
					Ready: true,
				},
			},
			&signup.Signup{
				Name:              alice,
				APIEndpoint:       testServer.URL,
				ClusterName:       "member-2",
				CompliantUsername: alice,
				Username:          "alice@",
				Status: signup.Status{
					Ready: true,
				},
			},
			&signup.Signup{
				Name:              john,
				CompliantUsername: john,
				Username:          "john@",
				Status: signup.Status{
					Ready: false,
				},
			},
			&signup.Signup{
				Name:              eve,
				CompliantUsername: eve,
				Username:          "eve@",
				Status: signup.Status{
					Ready:  false,
					Reason: toolchainv1alpha1.UserSignupUserBannedReason,
				},
			},
		)

		// init fakeClient
		cli := commontest.NewFakeClient(s.T(),
			fake.NewSpace("smith-community", "member-2", "smith"),
			fake.NewSpace("alice-private", "member-2", "alice"),
			fake.NewSpaceBinding("smith-community-smith", "smith", "smith-community", "admin"),
			fake.NewSpaceBinding("smith-community-publicviewer", toolchainv1alpha1.KubesawAuthenticatedUsername, "smith-community", "viewer"),
			fake.NewSpaceBinding("alice-default", "alice", "alice-private", "admin"),
			fake.NewBannedUser("eve", eveEmail),
			fake.NewBase1NSTemplateTier(),
		)

		// configure proxy to the latest mocks
		proxy.Client.Client = cli
		proxy.getMembersFunc = s.newMemberClustersFunc(testServer.URL)
		proxy.signupService = signupService

		// configure proxy
		proxy.spaceLister = &handlers.SpaceLister{
			Client:        proxy.Client,
			GetSignupFunc: proxy.signupService.GetSignup,
			ProxyMetrics:  proxy.metrics,
		}

		// run test cases
		tests := map[string]struct {
			ProxyRequestMethod              string
			ProxyRequestHeaders             http.Header
			ExpectedAPIServerRequestHeaders http.Header
			ExpectedProxyResponseStatus     int
			RequestPath                     string
			ExpectedResponse                string
		}{
			// Given smith owns a workspace named smith-community
			// And   smith-community is publicly visible (shared with PublicViewer)
			// When  smith requests the list of pods in workspace smith-community
			// Then  the request is forwarded from the proxy
			// And   the request impersonates smith
			// And   the request's X-SSO-User Header is set to smith's ID
			// And   the request is successful
			"plain http actual request as community space owner": {
				ProxyRequestMethod:  "GET",
				ProxyRequestHeaders: map[string][]string{"Authorization": {"Bearer " + s.token(smith)}},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Authorization":    {"Bearer clusterSAToken"},
					"Impersonate-User": {"smith"},
					"X-SSO-User":       {smith},
				},
				ExpectedProxyResponseStatus: http.StatusOK,
				RequestPath:                 podsRequestURL("smith-community"),
				ExpectedResponse:            httpTestServerResponse,
			},
			// Given The not ready user john exists
			// When  john requests the list of pods in workspace smith-community
			// Then  the request is forwarded from the proxy
			// And   the request impersonates john
			// And   the request's X-SSO-User Header is set to john's ID
			// And   the request is successful
			"plain http actual request as notReadyUser": {
				ProxyRequestMethod:  "GET",
				ProxyRequestHeaders: map[string][]string{"Authorization": {"Bearer " + s.token(john)}},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Authorization":    {"Bearer clusterSAToken"},
					"Impersonate-User": {toolchainv1alpha1.KubesawAuthenticatedUsername},
					"X-SSO-User":       {john},
				},
				ExpectedProxyResponseStatus: http.StatusOK,
				RequestPath:                 podsRequestURL("smith-community"),
				ExpectedResponse:            httpTestServerResponse,
			},
			// Given The not signed up user bob exists
			// When  bob requests the list of pods in workspace smith-community
			// Then  the request is forwarded from the proxy
			// And   the request impersonates bob
			// And   the request's X-SSO-User Header is set to bob's ID
			// And   the request is successful
			"plain http actual request as not signed up user": {
				ProxyRequestMethod:  "GET",
				ProxyRequestHeaders: map[string][]string{"Authorization": {"Bearer " + s.token(bob)}},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Authorization":    {"Bearer clusterSAToken"},
					"Impersonate-User": {toolchainv1alpha1.KubesawAuthenticatedUsername},
					"X-SSO-User":       {bob},
				},
				ExpectedProxyResponseStatus: http.StatusOK,
				RequestPath:                 podsRequestURL("smith-community"),
				ExpectedResponse:            httpTestServerResponse,
			},
			// Given smith owns a workspace named smith-community
			// And   smith-community is publicly visible (shared with PublicViewer)
			// And   a user named alice exists
			// And   smith's smith-community is not directly shared with alice
			// When  alice requests the list of pods in workspace smith-community
			// Then  the request is forwarded from the proxy
			// And   the request impersonates the PublicViewer
			// And   the request's X-SSO-User Header is set to alice's ID
			// And   the request is successful
			"plain http actual request as community user": {
				ProxyRequestMethod:  "GET",
				ProxyRequestHeaders: map[string][]string{"Authorization": {"Bearer " + s.token(alice)}},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Authorization":    {"Bearer clusterSAToken"},
					"Impersonate-User": {toolchainv1alpha1.KubesawAuthenticatedUsername},
					"X-SSO-User":       {alice},
				},
				ExpectedProxyResponseStatus: http.StatusOK,
				RequestPath:                 podsRequestURL("smith-community"),
				ExpectedResponse:            httpTestServerResponse,
			},
			// Given user alice exists
			// And   alice owns a private workspace
			// When  smith requests the list of pods in alice's workspace
			// Then  the proxy does NOT forward the request
			// And   the proxy rejects the call with 403 Forbidden
			"plain http actual request as non-owner to private workspace": {
				ProxyRequestMethod:          "GET",
				ProxyRequestHeaders:         map[string][]string{"Authorization": {"Bearer " + s.token(smith)}},
				ExpectedProxyResponseStatus: http.StatusForbidden,
				RequestPath:                 podsRequestURL("alice-private"),
				ExpectedResponse:            "invalid workspace request: access to workspace 'alice-private' is forbidden",
			},
			// Given banned user eve exists
			// And   user smith exists
			// And   smith owns a workspace named smith-community
			// And   smith-community is publicly visible (shared with PublicViewer)
			// When  eve requests the list of pods in smith's workspace
			// Then  the proxy does NOT forward the request
			// And   the proxy rejects the call with 403 Forbidden
			"plain http actual request as banned user to community workspace": {
				ProxyRequestMethod:          "GET",
				ProxyRequestHeaders:         map[string][]string{"Authorization": {"Bearer " + s.token(eve, authsupport.WithEmailClaim(eveEmail))}},
				ExpectedProxyResponseStatus: http.StatusForbidden,
				RequestPath:                 podsRequestURL("smith-community"),
				ExpectedResponse:            "user access is forbidden: user access is forbidden",
			},
			// Given banned user eve exist
			// And   user alice exists
			// And   alice owns a private workspace
			// When  eve requests the list of pods in alice's workspace
			// Then  the proxy does NOT forward the request
			// And   the proxy rejects the call with 403 Forbidden
			"plain http actual request as banned user to private workspace": {
				ProxyRequestMethod:          "GET",
				ProxyRequestHeaders:         map[string][]string{"Authorization": {"Bearer " + s.token(eve, authsupport.WithEmailClaim(eveEmail))}},
				ExpectedProxyResponseStatus: http.StatusForbidden,
				RequestPath:                 podsRequestURL("alice-private"),
				ExpectedResponse:            "user access is forbidden: user access is forbidden",
			},
			// Given user alice exists
			// And   alice owns a private workspace
			// When  alice requests the list of pods in a namespace which does not belong to the alice's workspace
			// Then  the proxy does forward the request anyway.
			// It's not up to the proxy to check permissions on the specific namespace.
			// The target API server will reject the request if the user does not have permissions to access the namespace.
			// Here the request is successful because the underlying mock target cluster API always server returns OK
			"plain http request as permitted user to namespace outside of private workspace": {
				ProxyRequestMethod:  "GET",
				ProxyRequestHeaders: map[string][]string{"Authorization": {"Bearer " + s.token(alice)}},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Authorization":    {"Bearer clusterSAToken"},
					"Impersonate-User": {"alice"},
					"X-SSO-User":       {alice},
				},
				ExpectedProxyResponseStatus: http.StatusOK,
				RequestPath:                 podsInNamespaceRequestURL("alice-private", "outside-of-workspace-namespace"),
				ExpectedResponse:            httpTestServerResponse,
			},
			// Given smith owns a workspace named smith-community
			// And   smith-community is publicly visible (shared with PublicViewer)
			// When  smith requests the list of pods in a namespace which does not belong to the workspace smith-community
			// Then  the proxy does forward the request anyway.
			// It's not up to the proxy to check permissions on the specific namespace.
			// The target API server will reject the request if the user does not have permissions to access the namespace.
			// Here the request is successful because the underlying mock target cluster API server returns OK
			"plain http request as owner to namespace outside of community workspace": {
				ProxyRequestMethod:  "GET",
				ProxyRequestHeaders: map[string][]string{"Authorization": {"Bearer " + s.token(smith)}},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Authorization":    {"Bearer clusterSAToken"},
					"Impersonate-User": {"smith"},
					"X-SSO-User":       {smith},
				},
				ExpectedProxyResponseStatus: http.StatusOK,
				RequestPath:                 podsInNamespaceRequestURL("smith-community", "outside-of-workspace-namespace"),
				ExpectedResponse:            httpTestServerResponse,
			},
			// Given smith owns a workspace named smith-community
			// And   smith-community is publicly visible (shared with PublicViewer)
			// And   user alice exists
			// When  alice requests the list of pods in a namespace which does not belong to the smith's workspace
			// It's not up to the proxy to check permissions on the specific namespace.
			// The target API server will reject the request if the user does not have permissions to access the namespace.
			// Here the request is successful because the underlying mock target cluster API server returns OK
			"plain http request as community user to namespace outside of community workspace": {
				ProxyRequestMethod:  "GET",
				ProxyRequestHeaders: map[string][]string{"Authorization": {"Bearer " + s.token(alice)}},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Authorization":    {"Bearer clusterSAToken"},
					"Impersonate-User": {toolchainv1alpha1.KubesawAuthenticatedUsername},
					"X-SSO-User":       {alice},
				},
				ExpectedProxyResponseStatus: http.StatusOK,
				RequestPath:                 podsInNamespaceRequestURL("smith-community", "outside-of-workspace-namespace"),
				ExpectedResponse:            httpTestServerResponse,
			},
			// Given smith owns a workspace named smith-community
			// And   smith-community is publicly visible (shared with PublicViewer)
			// When  bob requests the list of pods in a namespace which does not belong to the smith's workspace
			// It's not up to the proxy to check permissions on the specific namespace.
			// The target API server will reject the request if the user does not have permissions to access the namespace.
			// Here the request is successful because the underlying mock target cluster API server returns OK
			"plain http request as unsigned user to namespace outside of community workspace": {
				ProxyRequestMethod:  "GET",
				ProxyRequestHeaders: map[string][]string{"Authorization": {"Bearer " + s.token(bob)}},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Authorization":    {"Bearer clusterSAToken"},
					"Impersonate-User": {toolchainv1alpha1.KubesawAuthenticatedUsername},
					"X-SSO-User":       {bob},
				},
				ExpectedProxyResponseStatus: http.StatusOK,
				RequestPath:                 podsInNamespaceRequestURL("smith-community", "outside-of-workspace-namespace"),
				ExpectedResponse:            httpTestServerResponse,
			},
			// Given smith owns a workspace named smith-community
			// And   smith-community is publicly visible (shared with PublicViewer)
			// And   not ready user john exists
			// When  john requests the list of pods in a namespace which does not belong to the smith's workspace
			// It's not up to the proxy to check permissions on the specific namespace.
			// The target API server will reject the request if the user does not have permissions to access the namespace.
			// Here the request is successful because the underlying mock target cluster API server returns OK
			"plain http request as notReadyUser to namespace outside community workspace": {
				ProxyRequestMethod:  "GET",
				ProxyRequestHeaders: map[string][]string{"Authorization": {"Bearer " + s.token(john)}},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Authorization":    {"Bearer clusterSAToken"},
					"Impersonate-User": {toolchainv1alpha1.KubesawAuthenticatedUsername},
					"X-SSO-User":       {john},
				},
				ExpectedProxyResponseStatus: http.StatusOK,
				RequestPath:                 podsInNamespaceRequestURL("smith-community", "outside-of-workspace-namespace"),
				ExpectedResponse:            httpTestServerResponse,
			},
			// Given banned user eve exists
			// And   user smith exists
			// And   smith owns a workspace named smith-community
			// And   smith-community is publicly visible (shared with PublicViewer)
			// When  eve requests the list of pods in a non-existing namespace smith's workspace
			// Then  the proxy does NOT forward the request
			// And   the proxy rejects the call with 403 Forbidden
			"plain http actual request as banned user to not existing namespace community workspace": {
				ProxyRequestMethod:          "GET",
				ProxyRequestHeaders:         map[string][]string{"Authorization": {"Bearer " + s.token(eve, authsupport.WithEmailClaim(eveEmail))}},
				ExpectedProxyResponseStatus: http.StatusForbidden,
				RequestPath:                 podsInNamespaceRequestURL("smith-community", "not-existing"),
				ExpectedResponse:            "user access is forbidden: user access is forbidden",
			},
		}

		for k, tc := range tests {
			s.Run(k, func() {
				testServerInvoked := atomic.NewBool(false)

				// given
				testServer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					v := testServerInvoked.Swap(true)
					assert.False(s.T(), v, "expected handler to be invoked just one time")

					w.Header().Set("Content-Type", "application/json")
					// Set the Access-Control-Allow-Origin header to make sure it's overridden by the proxy response modifier
					w.Header().Set("Access-Control-Allow-Origin", "dummy")
					w.WriteHeader(http.StatusOK)
					_, err := w.Write([]byte(httpTestServerResponse))
					assert.NoError(s.T(), err)
					for hk, hv := range tc.ExpectedAPIServerRequestHeaders {
						assert.Len(s.T(), r.Header.Values(hk), len(hv))
						for i := range hv {
							assert.Equal(s.T(), hv[i], r.Header.Values(hk)[i], "header %s", hk)
						}
					}
				})

				// prepare request
				req, err := http.NewRequest(tc.ProxyRequestMethod, tc.RequestPath, nil)
				require.NoError(s.T(), err)
				require.NotNil(s.T(), req)

				for hk, hv := range tc.ProxyRequestHeaders {
					for _, v := range hv {
						req.Header.Add(hk, v)
					}
				}

				// when
				client := http.Client{Timeout: 3 * time.Second}
				resp, err := client.Do(req)

				// then
				require.NoError(s.T(), err)
				require.NotNil(s.T(), resp)
				defer resp.Body.Close()
				assert.Equal(s.T(), tc.ExpectedProxyResponseStatus, resp.StatusCode)
				s.assertResponseBody(resp, tc.ExpectedResponse)

				forwardExpected := len(tc.ExpectedAPIServerRequestHeaders) > 0
				requestForwarded := testServerInvoked.Load()
				require.Equal(s.T(),
					forwardExpected, requestForwarded,
					"expecting call forward to be %v, got %v", forwardExpected, requestForwarded,
				)
			})
		}
	})
}
