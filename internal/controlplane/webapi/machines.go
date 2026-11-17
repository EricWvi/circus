package webapi

import (
	"context"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
)

// ListDevices lists the live devices the caller may use (deviceOwner), by
// name. Online comes from the live daemon links, not from the stored
// connection times: a process that died with links open leaves rows that
// still look connected.
func (s *Service) ListDevices(ctx context.Context, _ *connect.Request[circusv1.ListDevicesRequest]) (*connect.Response[circusv1.ListDevicesResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	out := &circusv1.ListDevicesResponse{Devices: []*circusv1.Device{}}
	owner, err := s.deviceOwner(ctx, u)
	if err != nil {
		return nil, s.internal(ctx, "device owner", err)
	}
	if owner == "" {
		return connect.NewResponse(out), nil
	}
	devices, err := s.st.ListIdentities(ctx, store.KindDevice, owner)
	if err != nil {
		return nil, s.internal(ctx, "list devices", err)
	}
	online, err := s.online(ctx, owner)
	if err != nil {
		return nil, s.internal(ctx, "online devices", err)
	}
	now := s.now().UnixMilli()
	for _, d := range devices {
		lastSeen := max(d.LastConnectedAt, d.LastDisconnectedAt)
		if online[d.ID] {
			lastSeen = now
		}
		out.Devices = append(out.Devices, &circusv1.Device{
			Name: d.Name, Online: online[d.ID], LastSeenUnixMs: lastSeen,
			Os: d.OS, Arch: d.Arch, Version: d.ClientVersion,
		})
	}
	return connect.NewResponse(out), nil
}
