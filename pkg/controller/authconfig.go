package controller

import (
	"net/http"

	"github.com/codeready-toolchain/registration-service/pkg/configuration"
	"github.com/gin-gonic/gin"
)

type configResponse struct {
	AuthClientLibraryURL string `json:"auth-client-library-url"`
	// this holds the raw config. Note: this is intentionally a string
	// not json as this field may also hold non-json configs!
	AuthClientConfigRaw string `json:"auth-client-config"`
	// The signup page URL. After user logs into SSO but has not signed up yet then this URL can be used to redirect the user to
	// sign up. If not set then the base URL of the registration service is supposed to be used by the clients.
	SignupURL string `json:"signup-url"`
}

// AuthConfig implements the auth config endpoint, which is invoked to
// retrieve the auth config for the ui.
type AuthConfig struct {
}

// NewAuthConfig returns a new AuthConfig instance.
func NewAuthConfig() *AuthConfig {
	return &AuthConfig{}
}

// GetHandler returns raw auth config content for UI.
func (ac *AuthConfig) GetHandler(ctx *gin.Context) {
	cfg := configuration.GetRegistrationServiceConfig()
	configRespData := configResponse{
		AuthClientLibraryURL: cfg.Auth().AuthClientLibraryURL(),
		AuthClientConfigRaw:  cfg.Auth().AuthClientConfigRaw(),
		SignupURL:            cfg.RegistrationServiceURL(),
	}
	ctx.JSON(http.StatusOK, configRespData)
}
