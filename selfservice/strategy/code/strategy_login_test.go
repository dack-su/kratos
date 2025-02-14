// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package code_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ory/x/stringsx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/ory/kratos/driver/config"
	"github.com/ory/kratos/identity"
	"github.com/ory/kratos/internal"
	"github.com/ory/kratos/internal/testhelpers"
	"github.com/ory/kratos/session"
	"github.com/ory/x/sqlxx"
)

func TestLoginCodeStrategy(t *testing.T) {
	ctx := context.Background()
	conf, reg := internal.NewFastRegistryWithMocks(t)
	testhelpers.SetDefaultIdentitySchema(conf, "file://./stub/code.identity.schema.json")
	conf.MustSet(ctx, fmt.Sprintf("%s.%s.enabled", config.ViperKeySelfServiceStrategyConfig, identity.CredentialsTypeCodeAuth.String()), false)
	conf.MustSet(ctx, fmt.Sprintf("%s.%s.passwordless_enabled", config.ViperKeySelfServiceStrategyConfig, identity.CredentialsTypeCodeAuth.String()), true)
	conf.MustSet(ctx, config.ViperKeySelfServiceBrowserDefaultReturnTo, "https://www.ory.sh")
	conf.MustSet(ctx, config.ViperKeyURLsAllowedReturnToDomains, []string{"https://www.ory.sh"})

	_ = testhelpers.NewLoginUIFlowEchoServer(t, reg)
	_ = testhelpers.NewErrorTestServer(t, reg)

	public, _, _, _ := testhelpers.NewKratosServerWithCSRFAndRouters(t, reg)

	createIdentity := func(ctx context.Context, t *testing.T, moreIdentifiers ...string) *identity.Identity {
		t.Helper()
		i := identity.NewIdentity(config.DefaultIdentityTraitsSchemaID)
		email := testhelpers.RandomEmail()

		ids := fmt.Sprintf(`"email":"%s"`, email)
		for i, identifier := range moreIdentifiers {
			ids = fmt.Sprintf(`%s,"email_%d":"%s"`, ids, i+1, identifier)
		}

		i.Traits = identity.Traits(fmt.Sprintf(`{%s}`, ids))

		credentials := map[identity.CredentialsType]identity.Credentials{
			identity.CredentialsTypePassword: {Identifiers: append([]string{email}, moreIdentifiers...), Type: identity.CredentialsTypePassword, Config: sqlxx.JSONRawMessage("{\"some\" : \"secret\"}")},
			identity.CredentialsTypeOIDC:     {Type: identity.CredentialsTypeOIDC, Identifiers: append([]string{email}, moreIdentifiers...), Config: sqlxx.JSONRawMessage("{\"some\" : \"secret\"}")},
			identity.CredentialsTypeWebAuthn: {Type: identity.CredentialsTypeWebAuthn, Identifiers: append([]string{email}, moreIdentifiers...), Config: sqlxx.JSONRawMessage("{\"some\" : \"secret\", \"user_handle\": \"rVIFaWRcTTuQLkXFmQWpgA==\"}")},
			identity.CredentialsTypeCodeAuth: {Type: identity.CredentialsTypeCodeAuth, Identifiers: append([]string{email}, moreIdentifiers...), Config: sqlxx.JSONRawMessage("{\"address_type\": \"email\", \"used_at\": \"2023-07-26T16:59:06+02:00\"}")},
		}
		i.Credentials = credentials

		var va []identity.VerifiableAddress
		for _, identifier := range moreIdentifiers {
			va = append(va, identity.VerifiableAddress{Value: identifier, Verified: false, Status: identity.VerifiableAddressStatusCompleted})
		}

		va = append(va, identity.VerifiableAddress{Value: email, Verified: true, Status: identity.VerifiableAddressStatusCompleted})

		i.VerifiableAddresses = va

		require.NoError(t, reg.PrivilegedIdentityPool().CreateIdentity(ctx, i))
		return i
	}

	type state struct {
		flowID        string
		identity      *identity.Identity
		client        *http.Client
		loginCode     string
		identityEmail string
		testServer    *httptest.Server
	}

	createLoginFlow := func(ctx context.Context, t *testing.T, public *httptest.Server, isSPA bool, moreIdentifiers ...string) *state {
		t.Helper()

		identity := createIdentity(ctx, t, moreIdentifiers...)

		client := testhelpers.NewClientWithCookies(t)
		client.Transport = testhelpers.NewTransportWithLogger(http.DefaultTransport, t).RoundTripper
		clientInit := testhelpers.InitializeLoginFlowViaBrowser(t, client, public, false, isSPA, false, false)

		body, err := json.Marshal(clientInit)
		require.NoError(t, err)

		csrfToken := gjson.GetBytes(body, "ui.nodes.#(attributes.name==csrf_token).attributes.value").String()
		require.NotEmpty(t, csrfToken)

		loginEmail := gjson.Get(identity.Traits.String(), "email").String()
		require.NotEmpty(t, loginEmail)

		return &state{
			flowID:        clientInit.GetId(),
			identity:      identity,
			identityEmail: loginEmail,
			client:        client,
			testServer:    public,
		}
	}

	type onSubmitAssertion func(t *testing.T, s *state, body string, res *http.Response)

	submitLogin := func(ctx context.Context, t *testing.T, s *state, isSPA bool, vals func(v *url.Values), mustHaveSession bool, submitAssertion onSubmitAssertion) *state {
		t.Helper()

		lf, resp, err := testhelpers.NewSDKCustomClient(s.testServer, s.client).FrontendApi.GetLoginFlow(ctx).Id(s.flowID).Execute()
		require.NoError(t, err)
		require.EqualValues(t, http.StatusOK, resp.StatusCode)

		values := testhelpers.SDKFormFieldsToURLValues(lf.Ui.Nodes)
		// we need to remove resend here
		// since it is not required for the first request
		// subsequent requests might need it later
		values.Del("resend")
		values.Set("method", "code")
		vals(&values)

		body, resp := testhelpers.LoginMakeRequest(t, false, isSPA, lf, s.client, testhelpers.EncodeFormAsJSON(t, false, values))

		if submitAssertion != nil {
			submitAssertion(t, s, body, resp)
			return s
		}

		if mustHaveSession {
			resp, err = s.client.Get(s.testServer.URL + session.RouteWhoami)
			require.NoError(t, err)
			require.EqualValues(t, http.StatusOK, resp.StatusCode)
		} else {
			// SPAs need to be informed that the login has not yet completed using status 400.
			// Browser clients will redirect back to the login URL.
			if isSPA {
				require.EqualValues(t, http.StatusBadRequest, resp.StatusCode)
			} else {
				require.EqualValues(t, http.StatusOK, resp.StatusCode)
			}
		}

		return s
	}

	for _, tc := range []struct {
		d     string
		isSPA bool
	}{
		{
			d:     "SPA client",
			isSPA: true,
		},
		{
			d:     "Browser client",
			isSPA: false,
		},
	} {
		t.Run("test="+tc.d, func(t *testing.T) {
			t.Run("case=email identifier should be case insensitive", func(t *testing.T) {
				// create login flow
				s := createLoginFlow(ctx, t, public, tc.isSPA)

				// submit email
				s = submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
					v.Set("identifier", stringsx.ToUpperInitial(s.identityEmail))
				}, false, nil)

				message := testhelpers.CourierExpectMessage(ctx, t, reg, s.identityEmail, "Login to your account")
				assert.Contains(t, message.Body, "please login to your account by entering the following code")

				loginCode := testhelpers.CourierExpectCodeInMessage(t, message, 1)
				assert.NotEmpty(t, loginCode)

				// 3. Submit OTP
				submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
					v.Set("code", loginCode)
				}, true, nil)
			})

			t.Run("case=should be able to log in with code", func(t *testing.T) {
				// create login flow
				s := createLoginFlow(ctx, t, public, tc.isSPA)

				// submit email
				s = submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
					v.Set("identifier", s.identityEmail)
				}, false, nil)

				message := testhelpers.CourierExpectMessage(ctx, t, reg, s.identityEmail, "Login to your account")
				assert.Contains(t, message.Body, "please login to your account by entering the following code")

				loginCode := testhelpers.CourierExpectCodeInMessage(t, message, 1)
				assert.NotEmpty(t, loginCode)

				// 3. Submit OTP
				submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
					v.Set("code", loginCode)
				}, true, nil)
			})

			t.Run("case=should not be able to change submitted id on code submit", func(t *testing.T) {
				// create login flow
				s := createLoginFlow(ctx, t, public, tc.isSPA)

				// submit email
				s = submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
					v.Set("identifier", s.identityEmail)
				}, false, nil)

				message := testhelpers.CourierExpectMessage(ctx, t, reg, s.identityEmail, "Login to your account")
				assert.Contains(t, message.Body, "please login to your account by entering the following code")

				loginCode := testhelpers.CourierExpectCodeInMessage(t, message, 1)
				assert.NotEmpty(t, loginCode)

				// 3. Submit OTP
				s = submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
					v.Set("identifier", "not-"+s.identityEmail)
					v.Set("code", loginCode)
				}, false, func(t *testing.T, s *state, body string, resp *http.Response) {
					if tc.isSPA {
						require.EqualValues(t, http.StatusBadRequest, resp.StatusCode)
						assert.Contains(t, gjson.Get(body, "ui.messages.0.text").String(), "account does not exist or has not setup sign in with code")
					} else {
						require.EqualValues(t, http.StatusOK, resp.StatusCode)
						require.EqualValues(t, conf.SelfServiceFlowLoginUI(ctx).Path, resp.Request.URL.Path)

						lf, resp, err := testhelpers.NewSDKCustomClient(public, s.client).FrontendApi.GetLoginFlow(ctx).Id(s.flowID).Execute()
						require.NoError(t, err)
						require.EqualValues(t, http.StatusOK, resp.StatusCode)
						body, err := json.Marshal(lf)
						require.NoError(t, err)
						assert.Contains(t, gjson.GetBytes(body, "ui.messages.0.text").String(), "account does not exist or has not setup sign in with code")
					}
				})
			})

			t.Run("case=should not be able to proceed to code entry when the account is unknown", func(t *testing.T) {
				s := createLoginFlow(ctx, t, public, tc.isSPA)

				// submit email
				s = submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
					v.Set("identifier", testhelpers.RandomEmail())
				}, false, func(t *testing.T, s *state, body string, resp *http.Response) {
					if tc.isSPA {
						require.EqualValues(t, http.StatusBadRequest, resp.StatusCode)
						assert.Contains(t, gjson.Get(body, "ui.messages.0.text").String(), "account does not exist or has not setup sign in with code")
					} else {
						require.EqualValues(t, http.StatusOK, resp.StatusCode)
						require.EqualValues(t, conf.SelfServiceFlowLoginUI(ctx).Path, resp.Request.URL.Path)

						lf, resp, err := testhelpers.NewSDKCustomClient(public, s.client).FrontendApi.GetLoginFlow(ctx).Id(s.flowID).Execute()
						require.NoError(t, err)
						require.EqualValues(t, http.StatusOK, resp.StatusCode)
						body, err := json.Marshal(lf)
						require.NoError(t, err)
						assert.Contains(t, gjson.GetBytes(body, "ui.messages.0.text").String(), "account does not exist or has not setup sign in with code")
					}
				})
			})

			t.Run("case=should not be able to use valid code after 5 attempts", func(t *testing.T) {
				s := createLoginFlow(ctx, t, public, tc.isSPA)

				// submit email
				s = submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
					v.Set("identifier", s.identityEmail)
				}, false, nil)

				message := testhelpers.CourierExpectMessage(ctx, t, reg, s.identityEmail, "Login to your account")
				assert.Contains(t, message.Body, "please login to your account by entering the following code")
				loginCode := testhelpers.CourierExpectCodeInMessage(t, message, 1)
				assert.NotEmpty(t, loginCode)

				for i := 0; i < 5; i++ {
					// 3. Submit OTP
					s = submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
						v.Set("code", "111111")
						v.Set("identifier", s.identityEmail)
					}, false, func(t *testing.T, s *state, body string, resp *http.Response) {
						if tc.isSPA {
							require.EqualValues(t, http.StatusBadRequest, resp.StatusCode)
						} else {
							// in browser flows we redirect back to the login ui
							require.Equal(t, http.StatusOK, resp.StatusCode, "%s", body)
						}
						assert.Contains(t, gjson.Get(body, "ui.messages.0.text").String(), "The login code is invalid or has already been used")
					})
				}

				// 3. Submit OTP
				s = submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
					v.Set("code", loginCode)
					v.Set("identifier", s.identityEmail)
				}, false, func(t *testing.T, s *state, body string, resp *http.Response) {
					if tc.isSPA {
						require.EqualValues(t, http.StatusBadRequest, resp.StatusCode)
					} else {
						// in browser flows we redirect back to the login ui
						require.Equal(t, http.StatusOK, resp.StatusCode, "%s", body)
					}
					assert.Contains(t, gjson.Get(body, "ui.messages.0.text").String(), "The request was submitted too often.")
				})
			})

			t.Run("case=code should expire", func(t *testing.T) {
				ctx := context.Background()

				conf.MustSet(ctx, config.ViperKeySelfServiceStrategyConfig+".code.config.lifespan", "1ns")

				t.Cleanup(func() {
					conf.MustSet(ctx, config.ViperKeySelfServiceStrategyConfig+".code.config.lifespan", "1h")
				})

				s := createLoginFlow(ctx, t, public, tc.isSPA)

				// submit email
				s = submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
					v.Set("identifier", s.identityEmail)
				}, false, nil)

				message := testhelpers.CourierExpectMessage(ctx, t, reg, s.identityEmail, "Login to your account")
				assert.Contains(t, message.Body, "please login to your account by entering the following code")
				loginCode := testhelpers.CourierExpectCodeInMessage(t, message, 1)
				assert.NotEmpty(t, loginCode)

				submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
					v.Set("code", loginCode)
					v.Set("identifier", s.identityEmail)
				}, false, func(t *testing.T, s *state, body string, resp *http.Response) {
					if tc.isSPA {
						require.EqualValues(t, http.StatusGone, resp.StatusCode)
						require.Contains(t, gjson.Get(body, "error.reason").String(), "self-service flow expired 0.00 minutes ago")
					} else {
						// with browser clients we redirect back to the UI with a new flow id as a query parameter
						require.Equal(t, http.StatusOK, resp.StatusCode)
						require.Equal(t, conf.SelfServiceFlowLoginUI(ctx).Path, resp.Request.URL.Path)
						lf, _, err := testhelpers.NewSDKCustomClient(public, s.client).FrontendApi.GetLoginFlow(ctx).Id(resp.Request.URL.Query().Get("flow")).Execute()
						require.NoError(t, err)
						require.EqualValues(t, http.StatusOK, resp.StatusCode)

						body, err := json.Marshal(lf)
						require.NoError(t, err)
						assert.Contains(t, gjson.GetBytes(body, "ui.messages.0.text").String(), "flow expired 0.00 minutes ago")
					}
				})
			})

			t.Run("case=resend code should invalidate previous code", func(t *testing.T) {
				ctx := context.Background()

				s := createLoginFlow(ctx, t, public, tc.isSPA)

				s = submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
					v.Set("identifier", s.identityEmail)
				}, false, nil)

				message := testhelpers.CourierExpectMessage(ctx, t, reg, s.identityEmail, "Login to your account")
				assert.Contains(t, message.Body, "please login to your account by entering the following code")
				loginCode := testhelpers.CourierExpectCodeInMessage(t, message, 1)
				assert.NotEmpty(t, loginCode)

				// resend code
				s = submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
					v.Set("resend", "code")
					v.Set("identifier", s.identityEmail)
				}, false, nil)

				message = testhelpers.CourierExpectMessage(ctx, t, reg, s.identityEmail, "Login to your account")
				assert.Contains(t, message.Body, "please login to your account by entering the following code")
				loginCode2 := testhelpers.CourierExpectCodeInMessage(t, message, 1)
				assert.NotEmpty(t, loginCode2)

				assert.NotEqual(t, loginCode, loginCode2)
				s = submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
					v.Set("code", loginCode)
					v.Set("identifier", s.identityEmail)
				}, false, func(t *testing.T, s *state, body string, res *http.Response) {
					if tc.isSPA {
						require.EqualValues(t, http.StatusBadRequest, res.StatusCode)
					} else {
						require.EqualValues(t, http.StatusOK, res.StatusCode)
					}
					require.Contains(t, gjson.Get(body, "ui.messages").String(), "The login code is invalid or has already been used. Please try again")
				})

				s = submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
					v.Set("code", loginCode2)
					v.Set("identifier", s.identityEmail)
				}, true, nil)
			})

			t.Run("case=on login with un-verified address, should verify it", func(t *testing.T) {
				s := createLoginFlow(ctx, t, public, tc.isSPA, testhelpers.RandomEmail())

				// we need to fetch only the first email
				loginEmail := gjson.Get(s.identity.Traits.String(), "email_1").String()
				require.NotEmpty(t, loginEmail)

				s.identityEmail = loginEmail

				var va *identity.VerifiableAddress

				for _, v := range s.identity.VerifiableAddresses {
					if v.Value == loginEmail {
						va = &v
						break
					}
				}

				require.NotNil(t, va)
				require.False(t, va.Verified)

				// submit email
				s = submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
					v.Set("identifier", s.identityEmail)
				}, false, nil)

				message := testhelpers.CourierExpectMessage(ctx, t, reg, loginEmail, "Login to your account")
				require.Contains(t, message.Body, "please login to your account by entering the following code")

				loginCode := testhelpers.CourierExpectCodeInMessage(t, message, 1)
				require.NotEmpty(t, loginCode)

				// Submit OTP
				s = submitLogin(ctx, t, s, tc.isSPA, func(v *url.Values) {
					v.Set("code", loginCode)
					v.Set("identifier", s.identityEmail)
				}, true, nil)

				id, err := reg.PrivilegedIdentityPool().GetIdentity(ctx, s.identity.ID, identity.ExpandEverything)
				require.NoError(t, err)

				va = nil

				for _, v := range id.VerifiableAddresses {
					if v.Value == loginEmail {
						va = &v
						break
					}
				}

				require.NotNil(t, va)
				require.True(t, va.Verified)
			})
		})
	}
}
