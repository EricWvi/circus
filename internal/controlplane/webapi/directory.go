package webapi

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
)

const maxUserDirectoryQueries = 8

// ListDirectory browses one of the signed-in user's devices.
func (s *Service) ListDirectory(ctx context.Context, req *connect.Request[circusv1.ListDirectoryRequest]) (*connect.Response[circusv1.ListDirectoryResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	path := req.Msg.GetPath()
	if len(path) > 4096 || strings.ContainsRune(path, 0) {
		return nil, invalidField("path", "path must be at most 4096 bytes and contain no NUL")
	}

	s.directoryMu.Lock()
	if s.directoryQueries[u.ID] >= maxUserDirectoryQueries {
		s.directoryMu.Unlock()
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("too many directory requests in flight"))
	}
	s.directoryQueries[u.ID]++
	s.directoryMu.Unlock()
	defer func() {
		s.directoryMu.Lock()
		if s.directoryQueries[u.ID]--; s.directoryQueries[u.ID] == 0 {
			delete(s.directoryQueries, u.ID)
		}
		s.directoryMu.Unlock()
	}()

	device, err := s.st.GetIdentityByName(ctx, store.KindDevice, u.ID, req.Msg.GetDevice())
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("device not found"))
	}
	if err != nil {
		return nil, s.internal(ctx, "look up directory query device", err)
	}
	result, err := s.query(ctx, device.ID, &circusv1.DeviceQuery{Query: &circusv1.DeviceQuery_ListDirectory{
		ListDirectory: &circusv1.ListDirectoryQuery{Path: path, IncludeHidden: req.Msg.GetIncludeHidden()},
	}})
	switch {
	case errors.Is(err, gateway.ErrDeviceOffline):
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, gateway.ErrQueryUnsupported), errors.Is(err, gateway.ErrQueryLinkClosed):
		return nil, connect.NewError(connect.CodeUnavailable, err)
	case errors.Is(err, gateway.ErrQueryBusy):
		return nil, connect.NewError(connect.CodeResourceExhausted, err)
	case err != nil:
		return nil, s.internal(ctx, "list device directory", err)
	}
	if listing := result.GetListDirectory(); listing != nil {
		return connect.NewResponse(&circusv1.ListDirectoryResponse{Result: listing}), nil
	}
	switch result.GetError().GetCode() {
	case circusv1.DeviceQueryError_CODE_BUSY:
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("the device is busy"))
	case circusv1.DeviceQueryError_CODE_SHUTTING_DOWN:
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the device is shutting down"))
	default:
		return nil, s.internal(ctx, "list device directory", errors.New("unexpected device query outcome: "+result.GetError().GetCode().String()))
	}
}
