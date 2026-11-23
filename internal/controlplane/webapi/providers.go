package webapi

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/providers"
	"github.com/EricWvi/circus/internal/store"
)

// ProviderService request and response limits. The largest request is a
// provider with a 4 KiB key, 64 KiB of variables and 256 worker ids, each
// byte up to six in JSON; the largest response is ListProviders: 32
// providers, each with variable names, workers and URLs but no secrets.
const (
	maxProviderRequestBytes  = 1 << 20
	maxProviderResponseBytes = 8 << 20
)

var (
	errProviderNotFound = errors.New("provider not found")
	errUnreadableKey    = "the stored secrets cannot be decrypted; enter the key again"
	errUnreadableValue  = "the stored secrets cannot be decrypted; enter every value again"
)

// Scheduler is what the user-facing API needs of the session scheduler
// beyond Waker: sessions.Service implements it.
type Scheduler interface {
	// QueueReason is why a queued session is not running yet.
	QueueReason(sessionID string) circusv1.QueueReason
	// WorkerDisconnectedAt is when the worker holding the session's lease
	// lost its last connection (Unix ms), 0 while it is connected, without
	// a lease, or unknown.
	WorkerDisconnectedAt(sessionID string) int64
	// ProviderChanged re-evaluates the queued sessions of a provider that
	// changed or was deleted.
	ProviderChanged(userID, providerID string)
}

type noScheduler struct{}

func (noScheduler) QueueReason(string) circusv1.QueueReason { return 0 }
func (noScheduler) WorkerDisconnectedAt(string) int64       { return 0 }
func (noScheduler) ProviderChanged(string, string)          {}

// providerIDArg checks a request's provider id. An empty one is NOT_FOUND
// like any other unknown id.
func providerIDArg(id string) error {
	if len(id) > store.MaxIDBytes {
		return invalidField("provider_id", "provider id is too long")
	}
	if id == "" {
		return connect.NewError(connect.CodeNotFound, errProviderNotFound)
	}
	return nil
}

func protoScheme(s store.ProviderScheme) circusv1.Provider_ApiEndpoint_Scheme {
	if s == store.ProviderSchemeBearer {
		return circusv1.Provider_ApiEndpoint_SCHEME_BEARER
	}
	return circusv1.Provider_ApiEndpoint_SCHEME_X_API_KEY
}

// protoProvider is p as the API shows it: no secret, only their last four
// characters.
func (s *Service) protoProvider(p store.Provider) *circusv1.Provider {
	out := &circusv1.Provider{
		ProviderId: p.ID, Name: p.Name, Kind: circusv1.Provider_KIND_CLAUDE_CODE,
		Api: &circusv1.Provider_ApiEndpoint{
			BaseUrl: p.BaseURL, Scheme: protoScheme(p.Scheme), KeyLast4: p.KeyLast4,
			OpusModel: p.OpusModel, SonnetModel: p.SonnetModel, HaikuModel: p.HaikuModel,
		},
		WorkerIds: append([]string{}, p.WorkerIDs...), MaxSessions: uint32(max(p.MaxSessions, 0)), IsDefault: p.IsDefault,
		LeasedSessions: uint32(max(p.LeasedSessions, 0)), SecretsUnreadable: !s.secrets.Readable(p),
		CreatedAtUnixMs: p.CreatedAt, UpdatedAtUnixMs: p.UpdatedAt,
	}
	for i, n := range p.EnvNames {
		out.Env = append(out.Env, &circusv1.Provider_EnvVar{Name: n, ValueLast4: p.EnvLast4[i]})
	}
	return out
}

// checkProviderName trims a provider name and returns it, or a short
// reason it is refused.
func checkProviderName(raw string) (string, string) {
	name := jsTrim(raw)
	switch {
	case name == "":
		return "", "enter a provider name"
	case utf8.RuneCountInString(name) > store.MaxProviderNameRunes:
		return "", "provider name is longer than 64 characters"
	case strings.ContainsFunc(name, store.IsForbiddenDisplayRune):
		return "", "provider name has control or invisible characters"
	}
	return name, ""
}

// checkBaseURL returns why raw may not be an API base URL, or "": empty,
// or, at most store.MaxProviderBaseURLBytes of printable ASCII (no space,
// no control character, no backslash; an international domain name is
// typed in its punycode form):
//
//   - the scheme http:// or https://, in any case;
//   - an authority without user info ("@"): a host, either a name or an
//     IPv6 address in brackets, then optionally ":" and a port of one to
//     five digits with a value of 1 to 65535. A name is at most 253
//     bytes of dot-separated labels of letters, digits, "-" and "_" (so
//     no "%"), each of 1 to 63 bytes, neither starting nor ending with
//     "-"; when its last
//     label is a number (decimal digits, or "0x" and hexadecimal digits,
//     which URL parsers read as part of an IPv4 address), the name must
//     be an IPv4 address of four decimal numbers 0 to 255 without leading
//     zeros;
//   - then nothing, or a path starting with "/" in which every "%" is
//     followed by two hexadecimal digits and none of the characters
//     " < > [ ] ^ ` { | } appears unescaped;
//   - no query ("?") and no fragment ("#") anywhere.
//
// The Web client implements the same rule; the parity fixture holds both
// to it, so it is spelled out here rather than left to net/url.
func checkBaseURL(raw string) string {
	if raw == "" {
		return ""
	}
	if len(raw) > store.MaxProviderBaseURLBytes {
		return "base URL is longer than 2048 bytes"
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c <= ' ' || c >= 0x7f || c == '\\' {
			if c >= 0x80 && inAuthority(raw, i) {
				return "base URL host must be ASCII: type an international domain name in its punycode form (xn--)"
			}
			return "base URL may hold only printable ASCII, without spaces or backslashes"
		}
	}
	if strings.ContainsAny(raw, "?#") {
		return "base URL must not have a query or a fragment"
	}
	var rest string
	switch lower := strings.ToLower(raw); {
	case strings.HasPrefix(lower, "https://"):
		rest = raw[len("https://"):]
	case strings.HasPrefix(lower, "http://"):
		rest = raw[len("http://"):]
	default:
		return "base URL must start with http:// or https://"
	}
	authority, path := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		authority, path = rest[:i], rest[i:]
	}
	if strings.Contains(authority, "@") {
		return "base URL must not hold user info"
	}
	host, port, hasPort := authority, "", false
	if strings.HasPrefix(authority, "[") {
		end := strings.IndexByte(authority, ']')
		if end < 0 {
			return "base URL has an unterminated IPv6 address"
		}
		addr, err := netip.ParseAddr(authority[1:end])
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return "base URL has an invalid IPv6 address"
		}
		host = authority[:end+1]
		switch after := authority[end+1:]; {
		case after == "":
		case after[0] == ':':
			port, hasPort = after[1:], true
		default:
			return "base URL has text after its IPv6 address"
		}
	} else if i := strings.IndexByte(authority, ':'); i >= 0 {
		host, port, hasPort = authority[:i], authority[i+1:], true
		if strings.Contains(port, ":") {
			return "base URL needs brackets around an IPv6 address"
		}
	}
	if host == "" {
		return "base URL needs a host"
	}
	if !strings.HasPrefix(host, "[") {
		if problem := checkHostName(host); problem != "" {
			return problem
		}
	}
	if hasPort {
		n := 0
		for i := 0; i < len(port); i++ {
			if port[i] < '0' || port[i] > '9' {
				return "base URL port must be a number"
			}
			n = n*10 + int(port[i]-'0')
		}
		if port == "" || len(port) > 5 || n < 1 || n > 65535 {
			return "base URL port must be 1 to 65535"
		}
	}
	for i := 0; i < len(path); i++ {
		if path[i] == '%' && (i+2 >= len(path) || !isHex(path[i+1]) || !isHex(path[i+2])) {
			return "base URL has a \"%\" not followed by two hexadecimal digits"
		}
		if strings.IndexByte(pathEscaped, path[i]) >= 0 {
			return "base URL path must percent-encode the characters \" < > [ ] ^ ` { | }"
		}
	}
	return ""
}

// pathEscaped are the printable ASCII characters (besides space, "\\",
// "?" and "#", refused on their own) a base URL's path must
// percent-encode.
const pathEscaped = "\"<>[]^`{|}"

// inAuthority reports whether byte i of raw lies in the authority of a URL
// starting with a scheme and "//" (none when raw does not).
func inAuthority(raw string, i int) bool {
	start := strings.Index(raw, "://")
	if start < 0 || i < start+3 {
		return false
	}
	end := strings.IndexAny(raw[start+3:], "/?#")
	return end < 0 || i < start+3+end
}

// checkHostName returns why host, a base URL's host other than a
// bracketed IPv6 address, is refused, or "" (see checkBaseURL).
func checkHostName(host string) string {
	if len(host) > 253 {
		return "base URL host is longer than 253 bytes"
	}
	labels := strings.Split(host, ".")
	for _, l := range labels {
		switch {
		case l == "":
			return "base URL host has an empty label (two dots in a row, or a dot at either end)"
		case len(l) > 63:
			return "base URL host has a label longer than 63 bytes"
		case l[0] == '-' || l[len(l)-1] == '-':
			return "base URL host has a label starting or ending with \"-\""
		}
		for i := 0; i < len(l); i++ {
			if c := l[i]; !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "base URL has an invalid host"
			}
		}
	}
	if numericLabel(labels[len(labels)-1]) {
		if addr, err := netip.ParseAddr(host); err != nil || !addr.Is4() {
			return "base URL host ends in a number but is not an IPv4 address"
		}
	}
	return ""
}

// numericLabel reports whether URL parsers read host label l as a number
// (an IPv4 part): decimal digits, or "0x" and hexadecimal digits.
func numericLabel(l string) bool {
	if len(l) >= 2 && l[0] == '0' && (l[1] == 'x' || l[1] == 'X') {
		for i := 2; i < len(l); i++ {
			if !isHex(l[i]) {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(l); i++ {
		if l[i] < '0' || l[i] > '9' {
			return false
		}
	}
	return l != ""
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

// checkModel returns why m may not be an alias model id, or "".
func checkModel(m string) string {
	switch {
	case len(m) > store.MaxProviderModelBytes:
		return "model id is longer than 256 bytes"
	case strings.ContainsFunc(m, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
		return "model id has whitespace or control characters"
	}
	return ""
}

// checkKey returns why a nonempty key is refused, or "": longer than
// store.MaxProviderKeyBytes, or holding anything but printable ASCII
// (0x21 to 0x7E, so no space): the worker sends the key in an HTTP
// header, which carries nothing else as typed.
func checkKey(k string) string {
	if len(k) > store.MaxProviderKeyBytes {
		return "key is longer than 4096 bytes"
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return "key may hold only printable ASCII, without spaces"
		}
	}
	return ""
}

// envInput is one requested variable; value nil keeps the stored one.
type envInput struct {
	name  string
	value *string
}

// providerInput is a checked provider request: everything but the
// secrets' merge, which needs the stored provider on update.
type providerInput struct {
	p   store.Provider // without secrets and their last four characters
	key string         // "" keeps the stored key (update only)
	env []envInput
}

func envField(i int, f string) string { return fmt.Sprintf("env[%d].%s", i, f) }

// checkProvider validates a requested provider for user u, reporting the
// first problem in field order. On create the key and every value are
// required.
func (s *Service) checkProvider(ctx context.Context, u store.User, m *circusv1.Provider, create bool) (providerInput, error) {
	var in providerInput
	name, problem := checkProviderName(m.GetName())
	if problem != "" {
		return in, invalidField("name", problem)
	}
	if m.GetKind() != circusv1.Provider_KIND_CLAUDE_CODE {
		return in, invalidField("kind", "kind must be Claude Code")
	}
	api := m.GetApi()
	if api == nil {
		return in, invalidField("api", "an API endpoint is required")
	}
	if problem := checkBaseURL(api.GetBaseUrl()); problem != "" {
		return in, invalidField("api.base_url", problem)
	}
	var scheme store.ProviderScheme
	switch api.GetScheme() {
	case circusv1.Provider_ApiEndpoint_SCHEME_X_API_KEY:
		scheme = store.ProviderSchemeXAPIKey
	case circusv1.Provider_ApiEndpoint_SCHEME_BEARER:
		scheme = store.ProviderSchemeBearer
	default:
		return in, invalidField("api.scheme", "scheme must be x-api-key or bearer")
	}
	if create && api.GetKey() == "" {
		return in, invalidField("api.key", "enter the key")
	}
	if problem := checkKey(api.GetKey()); problem != "" {
		return in, invalidField("api.key", problem)
	}
	for _, f := range []struct{ field, v string }{
		{"api.opus_model", api.GetOpusModel()}, {"api.sonnet_model", api.GetSonnetModel()}, {"api.haiku_model", api.GetHaikuModel()},
	} {
		if problem := checkModel(f.v); problem != "" {
			return in, invalidField(f.field, problem)
		}
	}
	if len(m.GetEnv()) > store.MaxProviderEnv {
		return in, invalidField("env", fmt.Sprintf("at most %d variables", store.MaxProviderEnv))
	}
	seen := make(map[string]bool, len(m.GetEnv()))
	for i, e := range m.GetEnv() {
		if problem := providers.CheckEnvName(e.GetName()); problem != "" {
			return in, invalidField(envField(i, "name"), problem)
		}
		if seen[e.GetName()] {
			return in, invalidField(envField(i, "name"), "variable is listed twice")
		}
		seen[e.GetName()] = true
		if create && e.Value == nil {
			return in, invalidField(envField(i, "value"), "enter a value")
		}
		if e.Value != nil && strings.ContainsRune(e.GetValue(), 0) {
			return in, invalidField(envField(i, "value"), "value has a NUL character")
		}
		in.env = append(in.env, envInput{name: e.GetName(), value: e.Value})
	}
	if create {
		if problem := envBytes(in.env, providers.Secrets{}); problem != "" {
			return in, invalidField("env", problem)
		}
	}
	if len(m.GetWorkerIds()) > store.MaxProviderWorkers {
		return in, invalidField("worker_ids", fmt.Sprintf("at most %d workers", store.MaxProviderWorkers))
	}
	var workers map[string]bool
	if len(m.GetWorkerIds()) > 0 {
		list, err := s.st.ListIdentities(ctx, store.KindWorker, u.ID)
		if err != nil {
			return in, s.internal(ctx, "list workers", err)
		}
		workers = make(map[string]bool, len(list))
		for _, w := range list {
			workers[w.ID] = true
		}
	}
	listed := make(map[string]bool, len(m.GetWorkerIds()))
	for i, w := range m.GetWorkerIds() {
		field := fmt.Sprintf("worker_ids[%d]", i)
		if !workers[w] {
			return in, invalidField(field, "unknown worker")
		}
		if listed[w] {
			return in, invalidField(field, "worker is listed twice")
		}
		listed[w] = true
	}
	if m.GetMaxSessions() > store.MaxProviderMaxSessions {
		return in, invalidField("max_sessions", fmt.Sprintf("max sessions is at most %d", store.MaxProviderMaxSessions))
	}
	in.key = api.GetKey()
	in.p = store.Provider{
		UserID: u.ID, Name: name, Kind: store.ProviderKindClaudeCode, BaseURL: api.GetBaseUrl(), Scheme: scheme,
		OpusModel: api.GetOpusModel(), SonnetModel: api.GetSonnetModel(), HaikuModel: api.GetHaikuModel(),
		WorkerIDs: append([]string{}, m.GetWorkerIds()...), MaxSessions: int(m.GetMaxSessions()), IsDefault: m.GetIsDefault(),
	}
	return in, nil
}

// envBytes returns why the variables, with kept values from stored, are
// over the byte budget, or "".
func envBytes(env []envInput, stored providers.Secrets) string {
	n := 0
	for _, e := range env {
		n += len(e.name)
		if e.value != nil {
			n += len(*e.value)
		} else if v, ok := stored.Value(e.name); ok {
			n += len(v)
		}
	}
	if n > store.MaxProviderEnvBytes {
		return "variables hold more than 64 KiB of names and values"
	}
	return ""
}

// seal completes in for provider id: the secrets merged with what is
// stored (old, readable or not), sealed unless they did not change, and
// their last four characters.
func (s *Service) seal(id string, in providerInput, old providers.Secrets, oldSealed []byte, readable bool) (store.Provider, error) {
	p := in.p
	p.ID = id
	next := providers.Secrets{Key: in.key}
	if next.Key == "" {
		if !readable {
			return p, invalidField("api.key", errUnreadableKey)
		}
		next.Key = old.Key
	}
	for i, e := range in.env {
		v := e.value
		if v == nil {
			if !readable {
				return p, invalidField(envField(i, "value"), errUnreadableValue)
			}
			stored, ok := old.Value(e.name)
			if !ok {
				return p, invalidField(envField(i, "value"), "enter a value for this new variable")
			}
			v = &stored
		}
		next.Env = append(next.Env, providers.EnvValue{Name: e.name, Value: *v})
		p.EnvNames = append(p.EnvNames, e.name)
		p.EnvLast4 = append(p.EnvLast4, providers.Last4(*v))
	}
	if problem := envBytes(in.env, old); problem != "" {
		return p, invalidField("env", problem)
	}
	p.KeyLast4 = providers.Last4(next.Key)
	if readable && next.Equal(old) {
		p.Secrets = oldSealed
		return p, nil
	}
	if s.secrets == nil {
		return p, errors.New("webapi: no provider key configured")
	}
	sealed, err := s.secrets.Seal(id, next)
	if err != nil {
		return p, err
	}
	p.Secrets = sealed
	return p, nil
}

// providerError maps the error of a provider write.
func (s *Service) providerError(ctx context.Context, op string, err error) error {
	var ce *connect.Error
	var we *store.ProviderWorkerError
	switch {
	case errors.As(err, &ce):
		return ce
	case ctx.Err() != nil:
		return s.internal(ctx, op, err)
	case errors.As(err, &we):
		// The worker was deleted after the request was checked.
		return invalidField(fmt.Sprintf("worker_ids[%d]", we.Index), "unknown worker")
	case errors.Is(err, store.ErrExists):
		return fieldError(connect.CodeAlreadyExists, "name", "a provider with this name already exists")
	case errors.Is(err, store.ErrProviderStale):
		return connect.NewError(connect.CodeAborted, errors.New("the provider changed since it was read"))
	case errors.Is(err, store.ErrLimit):
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("at most %d providers", store.MaxProvidersPerUser))
	case errors.Is(err, store.ErrProviderInUse):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("a session using this provider holds a lease; stop it or wait until it is released"))
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errProviderNotFound)
	}
	return s.internal(ctx, op, err)
}

func (s *Service) CreateProvider(ctx context.Context, req *connect.Request[circusv1.CreateProviderRequest]) (*connect.Response[circusv1.CreateProviderResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	in, err := s.checkProvider(ctx, u, req.Msg.GetProvider(), true)
	if err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, s.internal(ctx, "provider id", err)
	}
	p, err := s.seal(id, in, providers.Secrets{}, nil, true)
	if err != nil {
		return nil, s.providerError(ctx, "seal provider", err)
	}
	p.CreatedAt = s.now().UnixMilli()
	p.UpdatedAt = p.CreatedAt
	got, err := s.st.CreateProvider(ctx, p)
	if err != nil {
		return nil, s.providerError(ctx, "create provider", err)
	}
	return connect.NewResponse(&circusv1.CreateProviderResponse{Provider: s.protoProvider(got)}), nil
}

func (s *Service) ListProviders(ctx context.Context, _ *connect.Request[circusv1.ListProvidersRequest]) (*connect.Response[circusv1.ListProvidersResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	ps, err := s.st.ListProviders(ctx, u.ID, s.now().UnixMilli())
	if err != nil {
		return nil, s.internal(ctx, "list providers", err)
	}
	out := &circusv1.ListProvidersResponse{Providers: make([]*circusv1.Provider, 0, len(ps))}
	for _, p := range ps {
		out.Providers = append(out.Providers, s.protoProvider(p))
	}
	return connect.NewResponse(out), nil
}

func (s *Service) GetProvider(ctx context.Context, req *connect.Request[circusv1.GetProviderRequest]) (*connect.Response[circusv1.GetProviderResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	if err := providerIDArg(req.Msg.GetProviderId()); err != nil {
		return nil, err
	}
	p, err := s.st.GetProvider(ctx, u.ID, req.Msg.GetProviderId(), s.now().UnixMilli())
	if err != nil {
		return nil, s.providerError(ctx, "get provider", err)
	}
	return connect.NewResponse(&circusv1.GetProviderResponse{Provider: s.protoProvider(p)}), nil
}

func (s *Service) UpdateProvider(ctx context.Context, req *connect.Request[circusv1.UpdateProviderRequest]) (*connect.Response[circusv1.UpdateProviderResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg.GetProvider()
	id := m.GetProviderId()
	if err := providerIDArg(id); err != nil {
		return nil, err
	}
	in, err := s.checkProvider(ctx, u, m, false)
	if err != nil {
		return nil, err
	}
	var expected *int64
	if req.Msg.ExpectedUpdatedAtUnixMs != nil {
		expected = new(req.Msg.GetExpectedUpdatedAtUnixMs())
	}
	var got store.Provider
	changed := false
	err = s.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		now := s.now().UnixMilli()
		cur, err := tx.GetProvider(ctx, u.ID, id, now)
		if err != nil {
			return err
		}
		if expected != nil && *expected != cur.UpdatedAt {
			return store.ErrProviderStale
		}
		old, openErr := s.secrets.OpenProvider(cur)
		p, err := s.seal(id, in, old, cur.Secrets, openErr == nil)
		if err != nil {
			return err
		}
		p.CreatedAt, p.UpdatedAt = cur.CreatedAt, now
		if got, err = tx.UpdateProvider(ctx, u.ID, p, expected); err != nil {
			return err
		}
		changed = got.UpdatedAt != cur.UpdatedAt
		return nil
	})
	if err != nil {
		return nil, s.providerError(ctx, "update provider", err)
	}
	if changed {
		// Assignments, the cap or the secrets may let queued sessions run
		// or keep them waiting; leases are never revoked for it.
		s.scheduler.ProviderChanged(u.ID, id)
	}
	return connect.NewResponse(&circusv1.UpdateProviderResponse{Provider: s.protoProvider(got)}), nil
}

func (s *Service) DeleteProvider(ctx context.Context, req *connect.Request[circusv1.DeleteProviderRequest]) (*connect.Response[circusv1.DeleteProviderResponse], error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	id := req.Msg.GetProviderId()
	if err := providerIDArg(id); err != nil {
		return nil, err
	}
	if err := s.st.DeleteProvider(ctx, u.ID, id, s.now().UnixMilli()); err != nil {
		return nil, s.providerError(ctx, "delete provider", err)
	}
	// Queued sessions naming it now wait with PROVIDER_DELETED.
	s.scheduler.ProviderChanged(u.ID, id)
	return connect.NewResponse(&circusv1.DeleteProviderResponse{}), nil
}

// resolveProvider returns the provider a new session of project p uses:
// requested when set ("" for local login; otherwise one of u's providers,
// else INVALID_ARGUMENT "provider_id"), else the project's default, else
// the user's, else local login. A default naming a deleted provider counts
// as unset.
func resolveProvider(ctx context.Context, tx store.Ops, u store.User, p store.Project, requested *string, now int64) (string, error) {
	exists := func(id string) (bool, error) {
		_, err := tx.GetProvider(ctx, u.ID, id, now)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return err == nil, err
	}
	if requested != nil {
		if *requested == "" {
			return "", nil
		}
		ok, err := exists(*requested)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", invalidField("provider_id", "unknown provider")
		}
		return *requested, nil
	}
	if p.DefaultProviderID != "" {
		ok, err := exists(p.DefaultProviderID)
		if err != nil || ok {
			return p.DefaultProviderID, err
		}
	}
	stored, err := tx.GetUser(ctx, u.ID)
	if err != nil {
		return "", err
	}
	if stored.DefaultProviderID != "" {
		ok, err := exists(stored.DefaultProviderID)
		if err != nil || ok {
			return stored.DefaultProviderID, err
		}
	}
	return "", nil
}
