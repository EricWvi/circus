package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
)

// daemonAuth deliberately shares Server.authn, including its credential hash map.
type daemonAuth struct {
	authn *auth.Authenticator
	st    store.Store
}

func (a daemonAuth) Authenticate(ctx context.Context, r *http.Request) (gateway.Principal, error) {
	p, err := a.authn.Authenticate(ctx, r)
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		return gateway.Principal{}, gateway.ErrUnauthenticated
	case errors.Is(err, auth.ErrRevoked):
		return gateway.Principal{}, gateway.ErrRevoked
	case err != nil:
		return gateway.Principal{}, err
	case p.Kind != store.KindDevice:
		return gateway.Principal{}, fmt.Errorf("%w: %s credential supplied; use a daemon credential", gateway.ErrWrongRole, auth.RoleName(p.Kind))
	}
	deviceName := p.Name
	if deviceName == "" {
		// Compatibility with an auth adapter whose principal predates Name.
		id, err := a.st.GetIdentity(ctx, store.KindDevice, p.ID)
		if err != nil {
			return gateway.Principal{}, err
		}
		deviceName = id.Name
	}
	return gateway.Principal{UserID: p.UserID, DeviceID: p.ID, DeviceName: deviceName, CredentialID: p.CredentialID}, nil
}
func (a daemonAuth) Recheck(ctx context.Context, p gateway.Principal) error {
	valid, err := a.authn.Valid(ctx, p.CredentialID)
	if err != nil {
		return err
	}
	if !valid {
		return gateway.ErrRevoked
	}
	return nil
}
