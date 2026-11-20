package link

import (
	"context"
	"errors"
	"strings"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon/dirlist"
	"github.com/EricWvi/circus/internal/identifier"
	"github.com/EricWvi/circus/internal/wire"
)

const (
	maxDeviceQueries  = 4
	deviceQueryBudget = 5 * time.Second
	markerBudget      = min(2*time.Second, deviceQueryBudget-time.Second)
)

func (c *Client) deviceQuery(a *connection, q *circusv1.DeviceQuery) {
	id := q.GetRequestId()
	feature := wire.DeviceQueryFeature(q)
	switch {
	case !identifier.Valid(id, false):
		c.replyDeviceQuery(a, "", circusv1.DeviceQueryError_CODE_INVALID, nil)
	case c.stopping.Load():
		c.replyDeviceQuery(a, id, circusv1.DeviceQueryError_CODE_SHUTTING_DOWN, nil)
	case feature == "":
		c.replyDeviceQuery(a, id, circusv1.DeviceQueryError_CODE_UNSUPPORTED, nil)
	default:
		select {
		case c.querySlots <- struct{}{}:
			dir := q.GetListDirectory()
			path := q.GetGitRemote().GetPath()
			if dir != nil {
				path = dir.GetPath()
			}
			started := c.opts.Clock.Now()
			alarm := c.opts.Clock.After(deviceQueryBudget)
			go c.runDeviceQuery(a, strings.Clone(id), feature, strings.Clone(path), dir.GetIncludeHidden(), started, alarm)
		default:
			c.replyDeviceQuery(a, id, circusv1.DeviceQueryError_CODE_BUSY, nil)
		}
	}
}

func (c *Client) runDeviceQuery(a *connection, id, feature, path string, hidden bool, started time.Time, alarm <-chan time.Time) {
	defer func() { <-c.querySlots }()
	if a.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	stop := context.AfterFunc(c.queryCtx, cancel)
	defer stop()
	done := make(chan *circusv1.DeviceQueryResult, 1)
	go func() {
		result := &circusv1.DeviceQueryResult{RequestId: id}
		switch feature {
		case wire.FeatureGitRemote:
			result.Outcome = &circusv1.DeviceQueryResult_GitRemote{GitRemote: c.readGitRemote(ctx, path)}
		case wire.FeatureListDirectory:
			result.Outcome = &circusv1.DeviceQueryResult_ListDirectory{ListDirectory: c.directories.Read(ctx, path, hidden)}
		}
		done <- result
	}()

	reply := func(result *circusv1.DeviceQueryResult) {
		if c.stopping.Load() {
			c.replyDeviceQuery(a, id, circusv1.DeviceQueryError_CODE_SHUTTING_DOWN, nil)
			return
		}
		if result != nil {
			c.replyDeviceQuery(a, id, 0, result)
		}
	}
	timedOut := func() {
		result := &circusv1.DeviceQueryResult{RequestId: id}
		if feature == wire.FeatureGitRemote {
			result.Outcome = &circusv1.DeviceQueryResult_GitRemote{GitRemote: &circusv1.GitRemoteResult{
				Error: circusv1.GitRemoteError_GIT_REMOTE_ERROR_TIMEOUT,
			}}
		} else {
			listing := dirlist.Resolve(path)
			listing.Error = circusv1.DirectoryError_DIRECTORY_ERROR_TIMEOUT
			result.Outcome = &circusv1.DeviceQueryResult_ListDirectory{ListDirectory: listing}
		}
		reply(result)
	}
	select {
	case result := <-done:
		if !c.opts.Clock.Now().Before(started.Add(deviceQueryBudget)) {
			timedOut()
		} else {
			if listing := result.GetListDirectory(); listing != nil {
				c.directories.Mark(ctx, c.opts.Clock, started.Add(markerBudget), listing)
			}
			reply(result)
		}
		return
	case <-alarm:
		cancel()
		timedOut()
	case <-ctx.Done():
		reply(nil)
	}

	// A filesystem call can outlive the reply. Keep the slot until the worker
	// returns and any child processes have been reaped, even across reconnects.
	<-done
}

func (c *Client) replyDeviceQuery(a *connection, id string, code circusv1.DeviceQueryError_Code, reply *circusv1.DeviceQueryResult) {
	if reply == nil {
		reply = &circusv1.DeviceQueryResult{RequestId: id, Outcome: &circusv1.DeviceQueryResult_Error{
			Error: &circusv1.DeviceQueryError{Code: code},
		}}
	}
	envelope := &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_DeviceQueryResult{DeviceQueryResult: reply}}
	err := a.link.Send(a.ctx, envelope)
	if errors.Is(err, wire.ErrTooLarge) {
		// The link can reject a bounded result before enqueueing it.
		switch {
		case reply.GetListDirectory() != nil:
			result := reply.GetListDirectory()
			c.opts.Logger.Warn("directory listing exceeds link limit", "error", err)
			reply.Outcome = &circusv1.DeviceQueryResult_ListDirectory{ListDirectory: &circusv1.ListDirectoryResult{
				Path: result.Path, Parent: result.Parent, Home: result.Home, Error: circusv1.DirectoryError_DIRECTORY_ERROR_IO,
			}}
			err = a.link.Send(a.ctx, envelope)
		case reply.GetGitRemote().GetUrl() != "":
			c.opts.Logger.Warn("git remote result exceeds link limit", "error", err)
			reply.Outcome = &circusv1.DeviceQueryResult_GitRemote{GitRemote: &circusv1.GitRemoteResult{
				Error: circusv1.GitRemoteError_GIT_REMOTE_ERROR_FAILED,
			}}
			err = a.link.Send(a.ctx, envelope)
		}
	}
	if err != nil {
		c.opts.Logger.Debug("device query reply could not be sent", "error", err)
	}
}
