package storetest

import (
	"context"

	"github.com/EricWvi/circus/internal/store"
)

func (o *faultyOps) GetMachineUpdate(ctx context.Context, userID, machineID string) (v store.MachineUpdate, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.GetMachineUpdate(ctx, userID, machineID); return err })
	return
}
func (o *faultyOps) ListMachineUpdates(ctx context.Context, userID string) (v []store.MachineUpdate, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.ListMachineUpdates(ctx, userID); return err })
	return
}
func (o *faultyOps) PutMachineUpdate(ctx context.Context, u store.MachineUpdate) error {
	return o.write(ctx, func(x store.Ops) error { return x.PutMachineUpdate(ctx, u) })
}
func (o *faultyOps) GetMachineUpdateByID(ctx context.Context, updateID string) (v store.MachineUpdate, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.GetMachineUpdateByID(ctx, updateID); return err })
	return
}
func (o *faultyOps) FailMachineUpdate(ctx context.Context, updateID string, failure store.MachineUpdateFailure, errText string) (v store.MachineUpdate, ok bool, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, ok, err = x.FailMachineUpdate(ctx, updateID, failure, errText); return err })
	return
}
func (o *faultyOps) DeleteMachineUpdate(ctx context.Context, userID, machineID, updateID string) (v bool, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.DeleteMachineUpdate(ctx, userID, machineID, updateID); return err })
	return
}
