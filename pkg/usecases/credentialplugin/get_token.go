// Package credentialplugin provides the use-cases for running as a client-go credentials plugin.
//
// See https://kubernetes.io/docs/reference/access-authn-authz/authentication/#client-go-credential-plugins
package credentialplugin

import (
	"context"
	"strings"

	"github.com/int128/kubelogin/pkg/adaptors/mutex"

	"github.com/google/wire"
	"github.com/int128/kubelogin/pkg/adaptors/credentialpluginwriter"
	"github.com/int128/kubelogin/pkg/adaptors/logger"
	"github.com/int128/kubelogin/pkg/adaptors/tokencache"
	"github.com/int128/kubelogin/pkg/oidc"
	"github.com/int128/kubelogin/pkg/tlsclientconfig"
	"github.com/int128/kubelogin/pkg/usecases/authentication"
	"golang.org/x/xerrors"
)

//go:generate mockgen -destination mock_credentialplugin/mock_credentialplugin.go github.com/int128/kubelogin/pkg/usecases/credentialplugin Interface

var Set = wire.NewSet(
	wire.Struct(new(GetToken), "*"),
	wire.Bind(new(Interface), new(*GetToken)),
)

type Interface interface {
	Do(ctx context.Context, in Input) error
}

// Input represents an input DTO of the GetToken use-case.
type Input struct {
	IssuerURL       string
	ClientID        string
	ClientSecret    string
	ExtraScopes     []string // optional
	TokenCacheDir   string
	GrantOptionSet  authentication.GrantOptionSet
	TLSClientConfig tlsclientconfig.Config
}

type GetToken struct {
	Authentication       authentication.Interface
	TokenCacheRepository tokencache.Interface
	Writer               credentialpluginwriter.Interface
	Mutex                mutex.Interface
	Logger               logger.Interface
}

func (u *GetToken) Do(ctx context.Context, in Input) error {
	u.Logger.V(1).Infof("WARNING: log may contain your secrets such as token or password")

	// Prevent multiple concurrent token query using a file mutex. See https://github.com/int128/kubelogin/issues/389
	lock, err := u.Mutex.Acquire(ctx, "get-token")
	if err != nil {
		return err
	}
	defer func() {
		_ = u.Mutex.Release(lock)
	}()

	u.Logger.V(1).Infof("finding a token from cache directory %s", in.TokenCacheDir)
	tokenCacheKey := tokencache.Key{
		IssuerURL:      in.IssuerURL,
		ClientID:       in.ClientID,
		ClientSecret:   in.ClientSecret,
		ExtraScopes:    in.ExtraScopes,
		CACertFilename: strings.Join(in.TLSClientConfig.CACertFilename, ","),
		CACertData:     strings.Join(in.TLSClientConfig.CACertData, ","),
		SkipTLSVerify:  in.TLSClientConfig.SkipTLSVerify,
	}
	if in.GrantOptionSet.ROPCOption != nil {
		tokenCacheKey.Username = in.GrantOptionSet.ROPCOption.Username
	}
	cachedTokenSet, err := u.TokenCacheRepository.FindByKey(in.TokenCacheDir, tokenCacheKey)
	if err != nil {
		u.Logger.V(1).Infof("could not find a token cache: %s", err)
	}

	authenticationInput := authentication.Input{
		Provider: oidc.Provider{
			IssuerURL:    in.IssuerURL,
			ClientID:     in.ClientID,
			ClientSecret: in.ClientSecret,
			ExtraScopes:  in.ExtraScopes,
		},
		GrantOptionSet:  in.GrantOptionSet,
		CachedTokenSet:  cachedTokenSet,
		TLSClientConfig: in.TLSClientConfig,
	}
	authenticationOutput, err := u.Authentication.Do(ctx, authenticationInput)
	if err != nil {
		return xerrors.Errorf("authentication error: %w", err)
	}
	idTokenClaims, err := authenticationOutput.TokenSet.DecodeWithoutVerify()
	if err != nil {
		return xerrors.Errorf("you got an invalid token: %w", err)
	}
	u.Logger.V(1).Infof("you got a token: %s", idTokenClaims.Pretty)

	if authenticationOutput.AlreadyHasValidIDToken {
		u.Logger.V(1).Infof("you already have a valid token until %s", idTokenClaims.Expiry)
	} else {
		u.Logger.V(1).Infof("you got a valid token until %s", idTokenClaims.Expiry)
		if err := u.TokenCacheRepository.Save(in.TokenCacheDir, tokenCacheKey, authenticationOutput.TokenSet); err != nil {
			return xerrors.Errorf("could not write the token cache: %w", err)
		}
	}
	u.Logger.V(1).Infof("writing the token to client-go")
	out := credentialpluginwriter.Output{
		Token:  authenticationOutput.TokenSet.IDToken,
		Expiry: idTokenClaims.Expiry,
	}
	if err := u.Writer.Write(out); err != nil {
		return xerrors.Errorf("could not write the token to client-go: %w", err)
	}
	return nil
}
