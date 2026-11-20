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
	switch {
	case !identifier.Valid(id, false):
		c.replyDeviceQuery(a, "", circusv1.DeviceQueryError_CODE_INVALID, nil)
	case c.stopping.Load():
		c.replyDeviceQuery(a, id, circusv1.DeviceQueryError_CODE_SHUTTING_DOWN, nil)
	case q.GetListDirectory() == nil:
		c.replyDeviceQuery(a, id, circusv1.DeviceQueryError_CODE_UNSUPPORTED, nil)
	default:
		select {
		case c.querySlots <- struct{}{}:
			dir := q.GetListDirectory()
			started := c.opts.Clock.Now()
			alarm := c.opts.Clock.After(deviceQueryBudget)
			go c.runDirectoryQuery(a, strings.Clone(id), strings.Clone(dir.GetPath()), dir.GetIncludeHidden(), started, alarm)
		default:
			c.replyDeviceQuery(a, id, circusv1.DeviceQueryError_CODE_BUSY, nil)
		}
	}
}

func (c *Client) runDirectoryQuery(a *connection, id, path string, hidden bool, started time.Time, alarm <-chan time.Time) {
	defer func() { <-c.querySlots }()
	if a.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	stop := context.AfterFunc(c.queryCtx, cancel)
	defer stop()
	done := make(chan *circusv1.ListDirectoryResult, 1)
	go func() { done <- c.directories.Read(ctx, path, hidden) }()

	reply := func(result *circusv1.ListDirectoryResult) {
		if c.stopping.Load() {
			c.replyDeviceQuery(a, id, circusv1.DeviceQueryError_CODE_SHUTTING_DOWN, nil)
			return
		}
		if result != nil {
			c.replyDeviceQuery(a, id, 0, result)
		}
	}
	timedOut := func() {
		result := dirlist.Resolve(path)
		result.Error = circusv1.DirectoryError_DIRECTORY_ERROR_TIMEOUT
		reply(result)
	}
	select {
	case result := <-done:
		if !c.opts.Clock.Now().Before(started.Add(deviceQueryBudget)) {
			timedOut()
		} else {
			c.directories.Mark(ctx, c.opts.Clock, started.Add(markerBudget), result)
			reply(result)
		}
		return
	case <-alarm:
		cancel()
		timedOut()
	case <-ctx.Done():
		reply(nil)
	}

	// Only the directory read can outlive the reply while holding a query slot.
	// Keep that slot until the read returns, including across reconnects.
	<-done
}

func (c *Client) replyDeviceQuery(a *connection, id string, code circusv1.DeviceQueryError_Code, result *circusv1.ListDirectoryResult) {
	reply := &circusv1.DeviceQueryResult{RequestId: id}
	if result != nil {
		reply.Outcome = &circusv1.DeviceQueryResult_ListDirectory{ListDirectory: result}
	} else {
		reply.Outcome = &circusv1.DeviceQueryResult_Error{Error: &circusv1.DeviceQueryError{Code: code}}
	}
	envelope := &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_DeviceQueryResult{DeviceQueryResult: reply}}
	err := a.link.Send(a.ctx, envelope)
	if errors.Is(err, wire.ErrTooLarge) && result != nil {
		// A link may negotiate less space than a complete bounded listing uses.
		c.opts.Logger.Warn("directory listing exceeds link limit", "error", err)
		reply.Outcome = &circusv1.DeviceQueryResult_ListDirectory{ListDirectory: &circusv1.ListDirectoryResult{
			Path: result.Path, Parent: result.Parent, Home: result.Home, Error: circusv1.DirectoryError_DIRECTORY_ERROR_IO,
		}}
		err = a.link.Send(a.ctx, envelope)
	}
	if err != nil {
		c.opts.Logger.Debug("device query reply could not be sent", "error", err)
	}
}
