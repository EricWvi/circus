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

const maxUserDeviceQueries = 8

// ListDirectory browses one of the signed-in user's devices.
func (s *Service) ListDirectory(ctx context.Context, req *connect.Request[circusv1.ListDirectoryRequest]) (*connect.Response[circusv1.ListDirectoryResponse], error) {
	path := req.Msg.GetPath()
	result, err := s.queryDevice(ctx, req.Msg.GetDevice(), path, &circusv1.DeviceQuery{Query: &circusv1.DeviceQuery_ListDirectory{
		ListDirectory: &circusv1.ListDirectoryQuery{Path: path, IncludeHidden: req.Msg.GetIncludeHidden()},
	}})
	if err != nil {
		return nil, err
	}
	if listing := result.GetListDirectory(); listing != nil {
		return connect.NewResponse(&circusv1.ListDirectoryResponse{Result: listing}), nil
	}
	return nil, s.internal(ctx, "list device directory", errors.New("missing directory result"))
}

// GetGitRemote reads the origin URL of a repository on one of the user's devices.
func (s *Service) GetGitRemote(ctx context.Context, req *connect.Request[circusv1.GetGitRemoteRequest]) (*connect.Response[circusv1.GetGitRemoteResponse], error) {
	path := req.Msg.GetPath()
	result, err := s.queryDevice(ctx, req.Msg.GetDevice(), path, &circusv1.DeviceQuery{Query: &circusv1.DeviceQuery_GitRemote{
		GitRemote: &circusv1.GitRemoteQuery{Path: path},
	}})
	if err != nil {
		return nil, err
	}
	if remote := result.GetGitRemote(); remote != nil {
		return connect.NewResponse(&circusv1.GetGitRemoteResponse{Result: remote}), nil
	}
	return nil, s.internal(ctx, "get device git remote", errors.New("missing git remote result"))
}

func (s *Service) queryDevice(ctx context.Context, name, path string, query *circusv1.DeviceQuery) (*circusv1.DeviceQueryResult, error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	if len(path) > 4096 || strings.ContainsRune(path, 0) {
		return nil, invalidField("path", "path must be at most 4096 bytes and contain no NUL")
	}

	s.deviceQueryMu.Lock()
	if s.deviceQueries[u.ID] >= maxUserDeviceQueries {
		s.deviceQueryMu.Unlock()
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("too many device queries in flight"))
	}
	s.deviceQueries[u.ID]++
	s.deviceQueryMu.Unlock()
	defer func() {
		s.deviceQueryMu.Lock()
		if s.deviceQueries[u.ID]--; s.deviceQueries[u.ID] == 0 {
			delete(s.deviceQueries, u.ID)
		}
		s.deviceQueryMu.Unlock()
	}()

	device, err := s.st.GetIdentityByName(ctx, store.KindDevice, u.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("device not found"))
	}
	if err != nil {
		return nil, s.internal(ctx, "look up query device", err)
	}
	result, err := s.query(ctx, device.ID, query)
	switch {
	case errors.Is(err, gateway.ErrDeviceOffline):
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, gateway.ErrQueryUnsupported), errors.Is(err, gateway.ErrQueryLinkClosed):
		return nil, connect.NewError(connect.CodeUnavailable, err)
	case errors.Is(err, gateway.ErrQueryBusy):
		return nil, connect.NewError(connect.CodeResourceExhausted, err)
	case err != nil:
		return nil, s.internal(ctx, "query device", err)
	}
	if result.GetError() == nil {
		return result, nil
	}
	switch result.GetError().GetCode() {
	case circusv1.DeviceQueryError_CODE_BUSY:
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("the device is busy"))
	case circusv1.DeviceQueryError_CODE_SHUTTING_DOWN:
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("the device is shutting down"))
	default:
		return nil, s.internal(ctx, "query device", errors.New("unexpected device query outcome: "+result.GetError().GetCode().String()))
	}
}
