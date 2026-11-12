package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/EricWvi/circus/internal/credfile"
)

const doctorUsage = "usage: circus doctor [--role daemon|worker]"

// doctorCommand checks, for each role this machine has joined as, that the
// control plane is reachable and its TLS verifies, that it is ready, and that
// the credential is still accepted (D31 item 7, the minimal doctor).
func doctorCommand(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(errOut)
	only := fs.String("role", "", "check only this role")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New(doctorUsage)
	}
	roles := []credfile.Role{credfile.RoleDaemon, credfile.RoleWorker}
	if *only != "" {
		roles = []credfile.Role{credfile.Role(*only)}
	}
	found, failed := 0, 0
	for _, role := range roles {
		path, err := credfile.Path(role)
		if err != nil {
			return err
		}
		f, err := credfile.Load(path)
		if errors.Is(err, credfile.ErrNotFound) {
			if *only != "" {
				fmt.Fprintf(out, "[%s] no credential at %s\n", role, path)
				failed++
			}
			continue
		}
		found++
		if err != nil {
			fmt.Fprintf(out, "[%s] FAIL credential file: %v\n", role, err)
			failed++
			continue
		}
		if !checkRole(out, f) {
			failed++
		}
	}
	if found == 0 && *only == "" {
		return errors.New("no credentials on this machine; run `circus join` first")
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}

func checkRole(out io.Writer, f credfile.File) bool {
	fmt.Fprintf(out, "[%s] control plane %s, principal %s\n", f.Role, f.ControlPlaneURL, f.PrincipalID)
	// The file is user-editable: validate its URL like a join link before
	// any request carries the token, and use the normalized form.
	u, err := url.Parse(f.ControlPlaneURL)
	if err == nil {
		f.ControlPlaneURL, err = checkBaseURL(u)
	}
	if err != nil {
		fmt.Fprintf(out, "[%s] FAIL credential file control_plane_url: %v\n", f.Role, err)
		return false
	}
	client, err := httpClient(f.ControlPlaneURL, f.CAFile)
	if err != nil {
		fmt.Fprintf(out, "[%s] FAIL %v\n", f.Role, err)
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	get := func(path, token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.ControlPlaneURL+path, nil)
		if err != nil {
			return nil, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return client.Do(req)
	}

	resp, err := get("/healthz", "")
	if err != nil {
		fmt.Fprintf(out, "[%s] FAIL connect: %v%s\n", f.Role, err, tlsHint(err))
		return false
	}
	resp.Body.Close()
	fmt.Fprintf(out, "[%s] ok   reachable (healthz %d)\n", f.Role, resp.StatusCode)
	ok := resp.StatusCode == http.StatusOK

	if resp, err = get("/readyz", ""); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			fmt.Fprintf(out, "[%s] ok   ready\n", f.Role)
		} else {
			fmt.Fprintf(out, "[%s] FAIL not ready (readyz %d)\n", f.Role, resp.StatusCode)
			ok = false
		}
	}

	resp, err = get("/v1/whoami", f.Token)
	if err != nil {
		fmt.Fprintf(out, "[%s] FAIL credential check: %v\n", f.Role, err)
		return false
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var who struct {
			Role        string `json:"role"`
			PrincipalID string `json:"principal_id"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&who)
		if who.PrincipalID != f.PrincipalID || who.Role != string(f.Role) {
			fmt.Fprintf(out, "[%s] FAIL credential belongs to %s %s, not this file's principal\n", f.Role, who.Role, who.PrincipalID)
			return false
		}
		fmt.Fprintf(out, "[%s] ok   credential accepted\n", f.Role)
	case http.StatusUnauthorized:
		fmt.Fprintf(out, "[%s] FAIL credential unknown to this control plane (another control plane, or its database was replaced); get a new link and run `circus join --force`\n", f.Role)
		return false
	case http.StatusForbidden:
		fmt.Fprintf(out, "[%s] FAIL credential revoked; revocation also freed the name, so get a new link and run `circus join --force` to join again under the same name\n", f.Role)
		return false
	default:
		fmt.Fprintf(out, "[%s] FAIL credential check: HTTP %d\n", f.Role, resp.StatusCode)
		return false
	}
	return ok
}
