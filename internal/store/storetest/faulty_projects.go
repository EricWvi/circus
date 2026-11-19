package storetest

import (
	"context"

	"github.com/EricWvi/circus/internal/store"
)

func (o *faultyOps) CreateProject(ctx context.Context, p store.Project) (v store.Project, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.CreateProject(ctx, p); return err })
	return
}
func (o *faultyOps) GetProject(ctx context.Context, userID, projectID string) (v store.Project, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.GetProject(ctx, userID, projectID); return err })
	return
}
func (o *faultyOps) ListProjects(ctx context.Context, userID string) (v []store.Project, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.ListProjects(ctx, userID); return err })
	return
}
func (o *faultyOps) UpdateProject(ctx context.Context, userID, projectID string, patch store.ProjectPatch) (v store.Project, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.UpdateProject(ctx, userID, projectID, patch); return err })
	return
}
func (o *faultyOps) DeleteProject(ctx context.Context, userID, projectID string, now int64) error {
	return o.write(ctx, func(x store.Ops) error { return x.DeleteProject(ctx, userID, projectID, now) })
}
