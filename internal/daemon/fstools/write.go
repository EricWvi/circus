package fstools

import (
	"context"
	"fmt"
	"os"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

// Write is a stateless file-writing tool.
type Write struct{}

// NewWrite returns a stateless Write tool.
func NewWrite() *Write { return &Write{} }

// Name returns the device call tool name.
func (*Write) Name() string { return "write" }

// Run writes the complete file content and returns its resulting hash.
func (*Write) Run(ctx context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	r := call.GetWrite()
	path, _, err := resolve(r.GetPath(), r.GetCwd())
	if err != nil {
		return nil, err
	}
	unlock := lockPath(path)
	defer unlock()
	if rejection, err := symlinkRejection(path); err != nil {
		return toolError(err.Error()), nil
	} else if rejection != nil {
		return rejection, nil
	}
	data, info, err := load(path)
	created := os.IsNotExist(err)
	if err != nil && !created {
		// Inferred: uncaptured Write filesystem errors use tool-use-error with OS text.
		return toolError(err.Error()), nil
	}
	if changed(r.ExpectedSha256, data) {
		return toolError(modifiedError), nil
	}
	content := []byte(r.GetContent())
	if err := writeFile(ctx, path, content, info); err != nil {
		return toolError(err.Error()), nil
	}
	body := fmt.Sprintf("The file %s has been updated successfully.%s", r.GetPath(), currentContext)
	if created {
		body = fmt.Sprintf("File created successfully at: %s%s", r.GetPath(), currentContext)
	}
	result := output(body, false)
	result.Detail = &circusv1.DeviceResult_Write{Write: &circusv1.WriteResult{Sha256: hash(content), Created: created}}
	return result, nil
}
