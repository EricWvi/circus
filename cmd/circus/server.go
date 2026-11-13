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
	"github.com/EricWvi/circus/internal/controlplane"
	"github.com/EricWvi/circus/internal/store"
)

const serverUsage = `usage:
  circus server [--db PATH] [--listen HOST:PORT] (--tls-cert FILE --tls-key FILE | --behind-proxy | --dev-insecure)
                [--public-url URL] [--shutdown-timeout 10s] [--timer-poll-interval 1s]
                [--max-message-bytes N] [--device-grace 30s]
                [--heartbeat-interval 5s] [--heartbeat-timeout 15s]
  circus server enroll-token create --role daemon|worker|both [--ttl 1h] [--url https://host:port] [--db PATH]
  circus server device list|revoke <name> [--db PATH]
  circus server worker list|revoke <name> [--db PATH]

revoke revokes the identity's credentials and deletes it, freeing the name
for a new join. The admin commands open only an existing database: pass the
server's --db when it is not the default.

M2 supports one long-lived worker plus one CLI connection at a time. HTTP 503
on a worker link means both connection slots are full.`

// defaultListen is the default --listen address and the port enroll-token
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
	db := fs.String("db", defaultDBPath(), "SQLite database file")
	listen := fs.String("listen", defaultListen, "listen address host:port")
	cert := fs.String("tls-cert", "", "TLS certificate chain (PEM)")
	key := fs.String("tls-key", "", "TLS private key (PEM)")
	proxy := fs.Bool("behind-proxy", false, "serve plaintext behind a TLS-terminating reverse proxy")
	insecure := fs.Bool("dev-insecure", false, "serve plaintext; loopback addresses only")
	publicURL := fs.String("public-url", "", "base URL clients use (default: derived from each request)")
	shutdown := fs.Duration("shutdown-timeout", controlplane.DefaultShutdownTimeout, "graceful shutdown deadline")
	timerPoll := fs.Duration("timer-poll-interval", 0, "durable timer poll interval (default 1s)")
	maxMessage := fs.Int("max-message-bytes", 0, "link message limit in bytes (default 64 MiB)")
	grace := fs.Duration("device-grace", 0, "disconnected device grace (default 30s)")
	heartbeat := fs.Duration("heartbeat-interval", 0, "link heartbeat interval (default 5s)")
	heartbeatTimeout := fs.Duration("heartbeat-timeout", 0, "link heartbeat timeout (default 15s)")
	debug := fs.Bool("debug", false, "log at debug level")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New(serverUsage)
	}
	if *publicURL != "" {
		u, err := url.Parse(*publicURL)
		if err != nil {
			return fmt.Errorf("--public-url: %w", err)
		}
		if *publicURL, err = checkBaseURL(u); err != nil {
			return fmt.Errorf("--public-url: %w", err)
		}
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := openStore(ctx, *db)
	if err != nil {
		return err
	}
	defer st.Close()
	srv, err := controlplane.New(ctx, st, controlplane.Config{
		Listen: *listen, TLSCert: *cert, TLSKey: *key, BehindProxy: *proxy, DevInsecure: *insecure,
		PublicURL: *publicURL, ShutdownTimeout: *shutdown, TimerPollInterval: *timerPoll, LogOutput: errOut, LogLevel: level,
	})
	if err != nil {
		return err
	}
	if err := srv.ConfigureLinks(ctx, controlplane.LinkConfig{MaxMessageBytes: *maxMessage, GracePeriod: *grace, HeartbeatInterval: *heartbeat, HeartbeatTimeout: *heartbeatTimeout}); err != nil {
		return err
	}
	return srv.Run(ctx)
}

func enrollTokenCreate(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("server enroll-token create", flag.ContinueOnError)
	fs.SetOutput(errOut)
	db := fs.String("db", defaultDBPath(), "SQLite database file")
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
	st, err := openExistingStore(ctx, *db, errOut)
	if err != nil {
		return err
	}
	defer st.Close()
	now := time.Now()
	user, err := auth.EnsureUser(ctx, st, auth.DefaultUserName, now)
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
	db := fs.String("db", defaultDBPath(), "SQLite database file")
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	if sub == "revoke" && len(pos) != 1 || sub == "list" && len(pos) != 0 {
		return errors.New(serverUsage)
	}
	ctx := context.Background()
	st, err := openExistingStore(ctx, *db, errOut)
	if err != nil {
		return err
	}
	defer st.Close()
	user, err := auth.EnsureUser(ctx, st, auth.DefaultUserName, time.Now())
	if err != nil {
		return err
	}
	if sub == "list" {
		ids, err := st.ListIdentities(ctx, kind, user.ID)
		if err != nil {
			return err
		}
		return printIdentities(out, ids)
	}
	id, n, err := auth.RevokeByName(ctx, st, kind, user.ID, pos[0], time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Revoked %d credential(s) of %s %q (%s) and removed it; the name can join again.\n",
		n, auth.AdminNoun(kind), id.Name, id.ID)
	return nil
}

// printIdentities prints live identities as a table: name, id, machine id
// prefix, creation and last connection time.
func printIdentities(out io.Writer, ids []store.Identity) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tMACHINE\tCREATED\tLAST CONNECTED")
	for _, id := range ids {
		machine := id.MachineID
		if len(machine) > 12 {
			machine = machine[:12]
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", id.Name, id.ID, machine, formatMillis(id.CreatedAt), formatMillis(id.LastConnectedAt))
	}
	return tw.Flush()
}

func formatMillis(ms int64) string {
	if ms == 0 {
		return "never"
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
