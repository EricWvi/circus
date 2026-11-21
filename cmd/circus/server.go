package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/config"
	"github.com/EricWvi/circus/internal/controlplane"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/web"
)

const serverUsage = `usage:
  circus server [--config config.yaml] [--debug]
  circus server enroll-token create --user <email> --role daemon|worker|both [--ttl 1h] [--url https://host:port] [--config config.yaml]
  circus server device|worker list [--config config.yaml]
  circus server device|worker revoke --user <email> <name> [--config config.yaml]

All server settings, including worker_connections and the database path,
are read from the YAML file. See config.example.yaml. Admin commands open
only an existing database. Revoke frees the identity's name for a new join.`

// defaultListen is the default listen address and the port enroll-token
// create assumes without --url.
const (
	defaultListen = ":8443"
	defaultPort   = "8443"
)

func serverCommand(args []string, out, errOut io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "enroll-token":
			if len(args) < 2 || args[1] != "create" {
				return errors.New(serverUsage)
			}
			return enrollTokenCreate(args[2:], out, errOut)
		case "device":
			return identityCommand(store.KindDevice, args[1:], out, errOut)
		case "worker":
			return identityCommand(store.KindWorker, args[1:], out, errOut)
		case "help", "-h", "--help":
			fmt.Fprintln(out, serverUsage)
			return nil
		}
	}
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() { fmt.Fprintln(errOut, serverUsage); fs.PrintDefaults() }
	configPath := fs.String("config", "config.yaml", "YAML configuration file")
	debug := fs.Bool("debug", false, "log at debug level (overrides config)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New(serverUsage)
	}
	cfg, err := loadServerConfig(*configPath)
	if err != nil {
		return err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "debug" {
			cfg.Debug = *debug
		}
	})
	level := slog.LevelInfo
	if cfg.Debug {
		level = slog.LevelDebug
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := openStore(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer func() {
		// Close waits for connections still in use; an error means a store
		// user outlived the server's shutdown.
		if err := st.Close(); err != nil {
			slog.New(slog.NewJSONHandler(errOut, &slog.HandlerOptions{Level: level})).Error("store close failed", "error", err)
		}
	}()
	serverCfg := cfg.Server()
	serverCfg.UI = webui.FS()
	serverCfg.Version = version
	serverCfg.LogOutput, serverCfg.LogLevel = errOut, level
	srv, err := controlplane.New(ctx, st, serverCfg)
	if err != nil {
		// The gateway keeps the validation; name the config keys and
		// the minimum each pool fell below.
		var pool *gateway.PoolError
		if errors.As(err, &pool) {
			var parts []string
			if pool.Send < pool.MinSend {
				parts = append(parts, fmt.Sprintf("send_pool_bytes %d is below the minimum %d", pool.Send, pool.MinSend))
			}
			if pool.Receive < pool.MinReceive {
				parts = append(parts, fmt.Sprintf("receive_pool_bytes %d is below the minimum %d", pool.Receive, pool.MinReceive))
			}
			return errors.New(strings.Join(parts, "; ") + " (twice wire.MinBudget)")
		}
		return err
	}
	return srv.Run(ctx)
}

func enrollTokenCreate(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("server enroll-token create", flag.ContinueOnError)
	fs.SetOutput(errOut)
	configPath := fs.String("config", "config.yaml", "YAML configuration file")
	email := fs.String("user", "", "email of the Web user who will own the machines")
	role := fs.String("role", "", "role the link enrolls: daemon, worker or both (one token per role)")
	ttl := fs.Duration("ttl", auth.DefaultEnrollTTL, "token lifetime")
	base := fs.String("url", "", "public base URL of the control plane (default https://<hostname>:"+defaultPort+")")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New(serverUsage)
	}
	if store.NormalizeEmail(*email) == "" {
		return errors.New("--user <email> is required")
	}
	var kinds []store.PrincipalKind
	switch *role {
	case "daemon":
		kinds = []store.PrincipalKind{store.KindDevice}
	case "worker":
		kinds = []store.PrincipalKind{store.KindWorker}
	case "both":
		kinds = []store.PrincipalKind{store.KindDevice, store.KindWorker}
	case "":
		return errors.New("--role is required: daemon, worker or both")
	default:
		return fmt.Errorf("unknown role %q: use daemon, worker or both", *role)
	}
	if *base == "" {
		host, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("pass --url: %w", err)
		}
		*base = "https://" + host + ":" + defaultPort
		fmt.Fprintf(errOut, "note: no --url given; assuming %s\n", *base)
	}
	u, err := url.Parse(*base)
	if err != nil {
		return fmt.Errorf("--url: %w", err)
	}
	baseURL, err := checkBaseURL(u)
	if err != nil {
		return fmt.Errorf("--url: %w", err)
	}

	ctx := context.Background()
	cfg, err := loadServerConfig(*configPath)
	if err != nil {
		return err
	}
	st, err := openExistingStore(ctx, cfg.DB, errOut)
	if err != nil {
		return err
	}
	defer st.Close()
	now := time.Now()
	user, err := userByEmail(ctx, st, *email)
	if err != nil {
		return err
	}
	// One independent single-use token per role; a two-token link carries
	// the daemon token first (see joinLink in join.go).
	var tokens []string
	var expires int64
	for _, kind := range kinds {
		token, rec, err := auth.CreateEnrollToken(ctx, st, user.ID, kind, *ttl, now)
		if err != nil {
			return err
		}
		tokens, expires = append(tokens, token), rec.ExpiresAt
	}
	fmt.Fprintln(out, baseURL+"/join#"+strings.Join(tokens, "."))
	// The hint reads the link from stdin, so the token never appears in the
	// joining process's argv (/proc/<pid>/cmdline) or in shell history.
	hint := "circus join"
	if *role == "worker" {
		hint = "circus join --role worker"
	}
	fmt.Fprintf(errOut, "One-time %s enrollment link above, valid until %s. On the machine, run\n  %s\nand paste the link when asked (or pipe it in).\n",
		*role, time.UnixMilli(expires).Format(time.RFC3339), hint)
	return nil
}

// identityCommand runs `circus server device|worker list|revoke`.
func identityCommand(kind store.PrincipalKind, args []string, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] != "revoke" && args[0] != "list" {
		return errors.New(serverUsage)
	}
	sub := args[0]
	fs := flag.NewFlagSet("server "+auth.AdminNoun(kind)+" "+sub, flag.ContinueOnError)
	fs.SetOutput(errOut)
	configPath := fs.String("config", "config.yaml", "YAML configuration file")
	var email string
	if sub == "revoke" {
		fs.StringVar(&email, "user", "", "email of the Web user who owns the machine")
	}
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	if sub == "revoke" && len(pos) != 1 || sub == "list" && len(pos) != 0 {
		return errors.New(serverUsage)
	}
	if sub == "revoke" && store.NormalizeEmail(email) == "" {
		return errors.New("--user <email> is required")
	}
	ctx := context.Background()
	cfg, err := loadServerConfig(*configPath)
	if err != nil {
		return err
	}
	st, err := openExistingStore(ctx, cfg.DB, errOut)
	if err != nil {
		return err
	}
	defer st.Close()
	if sub == "list" {
		ids, err := st.ListIdentitySummaries(ctx, kind)
		if err != nil {
			return err
		}
		return printIdentities(out, ids)
	}
	user, err := userByEmail(ctx, st, email)
	if err != nil {
		return err
	}
	id, n, err := auth.RevokeByName(ctx, st, kind, user.ID, pos[0], time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Revoked %d credential(s) of %s %q (%s) and removed it; the name can join again.\n",
		n, auth.AdminNoun(kind), id.Name, id.ID)
	return nil
}

func userByEmail(ctx context.Context, st store.Store, email string) (store.User, error) {
	u, err := st.GetUserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, fmt.Errorf("unknown Web user %q", email)
	}
	return u, err
}

// printIdentities prints live identities with their owners and connection times.
func printIdentities(out io.Writer, ids []store.IdentitySummary) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tOWNER EMAIL\tID\tMACHINE\tCREATED\tLAST CONNECTED")
	for _, id := range ids {
		machine := id.MachineID
		if len(machine) > 12 {
			machine = machine[:12]
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", id.Name, id.OwnerEmail, id.ID, machine, formatMillis(id.CreatedAt), formatMillis(id.LastConnectedAt))
	}
	return tw.Flush()
}

func formatMillis(ms int64) string {
	if ms == 0 {
		return "never"
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func loadServerConfig(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return cfg, err
	}
	if cfg.DB == "" {
		cfg.DB = defaultDBPath()
	}
	return cfg, nil
}
