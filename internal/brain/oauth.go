package brain

import (
	"context"
	"errors"
	"net/http"
	"os"

	"connectrpc.com/connect"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/endpoints"

	brainv1 "github.com/focusd-so/brain/gen/brain/v1"
	commonv1 "github.com/focusd-so/brain/gen/common/v1"
	"github.com/google/go-github/v80/github"
)

func (s *ServiceImpl) OAuth2GetAuthorizationURL(ctx context.Context, req *connect.Request[brainv1.OAuth2GetAuthorizationURLRequest]) (*connect.Response[brainv1.OAuth2GetAuthorizationURLResponse], error) {
	redirectURI := os.Getenv("REDIRECT_URI")
	if redirectURI == "" {
		return nil, errors.New("missing redirect URI")
	}

	switch req.Msg.Provider {
	case "github":
		cfg, err := githubConfig()
		if err != nil {
			return nil, err
		}

		cfg.Scopes = req.Msg.Scopes

		opts := []oauth2.AuthCodeOption{
			oauth2.AccessTypeOffline,
		}

		if req.Msg.CodeChallenge != "" {
			opts = append(
				opts,
				oauth2.SetAuthURLParam("code_challenge", req.Msg.CodeChallenge),
				oauth2.SetAuthURLParam("code_challenge_method", "S256"),
			)
		}

		return connect.NewResponse(&brainv1.OAuth2GetAuthorizationURLResponse{
			Url: cfg.AuthCodeURL(req.Msg.State, opts...),
		}), nil

	case "slack":
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("slack support not yet implemented"))
	case "jira":
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("jira support not yet implemented"))
	case "google":
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("google support not yet implemented"))

	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid provider"))
	}
}

func (s *ServiceImpl) OAuth2ExchangeAuthorizationCode(ctx context.Context, req *connect.Request[brainv1.OAuth2ExchangeAuthorizationCodeRequest]) (*connect.Response[brainv1.OAuth2ExchangeAuthorizationCodeResponse], error) {
	switch req.Msg.Provider {
	case "github":
		cfg, err := githubConfig()
		if err != nil {
			return nil, err
		}

		opts := []oauth2.AuthCodeOption{}
		if req.Msg.CodeVerifier != "" {
			opts = append(opts, oauth2.VerifierOption(req.Msg.CodeVerifier))
		}

		token, err := cfg.Exchange(ctx, req.Msg.Code, opts...)
		if err != nil {
			return nil, err
		}

		return connect.NewResponse(&brainv1.OAuth2ExchangeAuthorizationCodeResponse{
			Token: &commonv1.OAuth2Token{
				AccessToken:  token.AccessToken,
				TokenType:    token.TokenType,
				RefreshToken: token.RefreshToken,
				ExpiryUnix:   token.Expiry.Unix(),
			},
		}), nil

	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid provider"))
	}
}

func (s *ServiceImpl) OAuth2RefreshAccessToken(ctx context.Context, req *connect.Request[brainv1.OAuth2RefreshAccessTokenRequest]) (*connect.Response[brainv1.OAuth2RefreshAccessTokenResponse], error) {
	switch req.Msg.Provider {
	case "github":
		// github tokens are not refreshable, they are revoked when the user revokes the authorization
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("github refresh not supported"))
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid provider"))
	}
}

func (s *ServiceImpl) OAuth2RevokeAccessToken(ctx context.Context, req *connect.Request[brainv1.OAuth2RevokeAccessTokenRequest]) (*connect.Response[brainv1.OAuth2RevokeAccessTokenResponse], error) {
	switch req.Msg.Provider {
	case "github":

		cfg, err := githubConfig()
		if err != nil {
			return nil, err
		}

		t := &BasicAuthTransport{
			Username: cfg.ClientID,
			Password: cfg.ClientSecret,
		}

		githubClient := github.NewClient(t.Client())

		if _, err := githubClient.Authorizations.Revoke(ctx, cfg.ClientID, req.Msg.Token); err != nil {
			return nil, err
		}

		return connect.NewResponse(&brainv1.OAuth2RevokeAccessTokenResponse{
			Success: true,
		}), nil

	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid provider"))
	}
}

func githubConfig() (*oauth2.Config, error) {
	clientID := os.Getenv("GITHUB_CLIENT_ID")
	clientSecret := os.Getenv("GITHUB_CLIENT_SECRET")

	if clientID == "" || clientSecret == "" {
		return nil, errors.New("missing GitHub client ID or client secret")
	}

	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  os.Getenv("REDIRECT_URI"),
		Endpoint:     endpoints.GitHub,
	}, nil
}

type BasicAuthTransport struct {
	Username string
	Password string
}

func (t *BasicAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.SetBasicAuth(t.Username, t.Password)
	return http.DefaultTransport.RoundTrip(req)
}

func (t *BasicAuthTransport) Client() *http.Client {
	return &http.Client{Transport: t}
}
