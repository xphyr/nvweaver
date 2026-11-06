// Command nvweaver manages an nvweaver service through its API. It only uses internal/client, so it can run on any machine that
// can reach the API, and can do nothing that the API can't.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"nvweaver/internal/client"
)

// version is set at build time, with -ldflags "-X main.version=...".
var version = "dev"

const (
	exitOK    = 0
	exitError = 1 // The request failed, or the API refused it.
	exitUsage = 2 // The command itself was wrong.
)

const usage = `nvweaver manages an nvweaver service through its API.

Usage:
  nvweaver <command> [arguments] [flags]

Commands:
  system                                      Show the service and target, and any warnings
  health                                      Check that the service can serve
  version                                     Show this program's version

  port      list | get ID | create | update ID | delete ID
  host      list | get NQN | create NQN | update NQN | delete NQN
  subsystem list | get NQN | create NQN | update NQN | delete NQN
            allow NQN HOST | deny NQN HOST | attach NQN PORT | detach NQN PORT
  namespace list NQN | get NQN NSID | create NQN | update NQN NSID | delete NQN NSID
  pool      list | get NAME | create NAME | update NAME | delete NAME
  volume    list | get NAME | usage NAME | create NAME | update NAME | delete NAME
  snapshot  list | get NAME | create NAME | delete NAME

Run "nvweaver <command> <action> --help" for an action's flags.

Flags for every command:
  --api URL          The API's address, or NVWEAVER_API_URL. Defaults to ` + client.DefaultURL + `
  --token-file FILE  A file holding the API's token. Otherwise, NVWEAVER_API_TOKEN is used
  --ca FILE          PEM certificates to trust for HTTPS, or NVWEAVER_CA
  --output FORMAT    table or json. Defaults to table. Also -o
  --timeout TIME     Limit on the whole command, such as 30s. Defaults to none

Sizes take a suffix of K, M, G, T or P, which are powers of 1024, as in --size 10G.

Exit status is 0 on success, 1 if the request failed, and 2 if the command itself was wrong.
`

// usageError is a mistake in the command, rather than a failed request.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

// run is everything but the process: it takes what the process has, and returns the exit status.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	args = hoistFlags(args)
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "help", "-h", "--help":
		_, _ = io.WriteString(stdout, usage)
		return exitOK
	case "version", "--version":
		_, _ = fmt.Fprintln(stdout, "nvweaver "+version)
		return exitOK
	}
	a := &app{ctx: ctx, stdout: stdout, stderr: stderr, getenv: getenv}
	err := a.dispatch(args)
	var ue usageError
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		return exitOK
	case errors.As(err, &ue):
		_, _ = fmt.Fprintf(stderr, "nvweaver: %s\nRun \"nvweaver help\" for what's available.\n", ue.msg)
		return exitUsage
	default:
		_, _ = fmt.Fprintf(stderr, "nvweaver: %v\n", err)
		return exitError
	}
}

// hoistFlags moves the flags that every command has from before the command to after it, where they're parsed, so that both
// "nvweaver --api URL system" and "nvweaver system --api URL" work. It leaves anything else alone, for the error about it.
func hoistFlags(args []string) []string {
	var flags []string
	for len(args) > 0 {
		name, _, hasValue := strings.Cut(strings.TrimLeft(args[0], "-"), "=")
		if !strings.HasPrefix(args[0], "-") || !globalFlags[name] {
			break
		}
		if hasValue || len(args) == 1 {
			flags = append(flags, args[0])
			args = args[1:]
		} else {
			flags = append(flags, args[0], args[1])
			args = args[2:]
		}
	}
	if len(args) == 0 {
		return flags
	}
	return append(args[:len(args):len(args)], flags...)
}

// globalFlags are the ones that app.flags adds, all of which take a value.
var globalFlags = map[string]bool{"api": true, "token-file": true, "ca": true, "output": true, "o": true, "timeout": true}

// app is one run of the command.
type app struct {
	ctx            context.Context
	stdout, stderr io.Writer
	getenv         func(string) string

	api, tokenFile, ca, output string
	timeout                    time.Duration
	client                     *client.Client
}

// flags returns a set with the flags that every command has.
func (a *app) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	fs.StringVar(&a.api, "api", "", "the API's address")
	fs.StringVar(&a.tokenFile, "token-file", "", "a file holding the API's token")
	fs.StringVar(&a.ca, "ca", "", "PEM certificates to trust for HTTPS")
	fs.StringVar(&a.output, "output", "table", "table or json")
	fs.StringVar(&a.output, "o", "table", "table or json")
	fs.DurationVar(&a.timeout, "timeout", 0, "limit on the whole command")
	return fs
}

// parse lets flags come before, between or after arguments, which the flag package alone doesn't, and checks the arguments' number.
func (a *app) parse(fs *flag.FlagSet, args []string, want ...string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usageError{err.Error()}
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != len(want) {
		return nil, usagef("%s takes %s", fs.Name(), describe(want))
	}
	if a.output != "table" && a.output != "json" {
		return nil, usagef("--output must be table or json, not %q", a.output)
	}
	return positional, a.connect()
}

func describe(want []string) string {
	if len(want) == 0 {
		return "no arguments"
	}
	return strings.Join(want, " ")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// connect makes the client from the flags and environment. A token is only taken from a file or the environment, as other
// users of the machine can see command lines.
func (a *app) connect() error {
	token := a.getenv("NVWEAVER_API_TOKEN")
	if a.tokenFile != "" {
		data, err := os.ReadFile(a.tokenFile)
		if err != nil {
			return fmt.Errorf("token file: %w", err)
		}
		token = strings.TrimSpace(string(data))
		if token == "" {
			return fmt.Errorf("token file %s is empty", a.tokenFile)
		}
	}
	c, err := client.New(client.Options{
		URL:    firstNonEmpty(a.api, a.getenv("NVWEAVER_API_URL")),
		Token:  token,
		CAFile: firstNonEmpty(a.ca, a.getenv("NVWEAVER_CA")),
	})
	if err != nil {
		return err
	}
	a.client = c
	if token != "" && clearTextOverNetwork(firstNonEmpty(a.api, a.getenv("NVWEAVER_API_URL"))) {
		_, _ = fmt.Fprintln(a.stderr, "nvweaver: warning: the token is crossing the network in clear text, as this isn't HTTPS")
	}
	if a.timeout > 0 {
		ctx, cancel := context.WithTimeout(a.ctx, a.timeout)
		context.AfterFunc(ctx, cancel)
		a.ctx = ctx
	}
	return nil
}

func clearTextOverNetwork(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())
}

func (a *app) dispatch(args []string) error {
	noun := args[0]
	rest := args[1:]
	if noun == "system" {
		return a.system(rest)
	}
	if noun == "health" {
		return a.health(rest)
	}
	actions, ok := map[string]map[string]func([]string) error{
		"port": {"list": a.portList, "get": a.portGet, "create": a.portCreate, "update": a.portUpdate, "delete": a.portDelete},
		"host": {"list": a.hostList, "get": a.hostGet, "create": a.hostCreate, "update": a.hostUpdate, "delete": a.hostDelete},
		"subsystem": {"list": a.subsystemList, "get": a.subsystemGet, "create": a.subsystemCreate, "update": a.subsystemUpdate,
			"delete": a.subsystemDelete, "allow": a.subsystemAllow, "deny": a.subsystemDeny, "attach": a.subsystemAttach, "detach": a.subsystemDetach},
		"namespace": {"list": a.namespaceList, "get": a.namespaceGet, "create": a.namespaceCreate, "update": a.namespaceUpdate, "delete": a.namespaceDelete},
		"pool":      {"list": a.poolList, "get": a.poolGet, "create": a.poolCreate, "update": a.poolUpdate, "delete": a.poolDelete},
		"volume":    {"list": a.volumeList, "get": a.volumeGet, "usage": a.volumeUsage, "create": a.volumeCreate, "update": a.volumeUpdate, "delete": a.volumeDelete},
		"snapshot":  {"list": a.snapshotList, "get": a.snapshotGet, "create": a.snapshotCreate, "delete": a.snapshotDelete},
	}[noun]
	if !ok {
		return usagef("unknown command %q", noun)
	}
	if len(rest) == 0 {
		return usagef("%s needs an action", noun)
	}
	action, ok := actions[rest[0]]
	if !ok {
		return usagef("%s has no action %q", noun, rest[0])
	}
	return action(rest[1:])
}

// emit prints something: as it is for JSON, or through the given function for a table.
func (a *app) emit(v any, table func(w io.Writer)) error {
	if a.output == "json" {
		enc := json.NewEncoder(a.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	table(w)
	return w.Flush()
}

// done reports something that has no result to show. JSON output stays empty, for scripts.
func (a *app) done(format string, args ...any) error {
	if a.output != "json" {
		_, _ = fmt.Fprintf(a.stdout, format+"\n", args...)
	}
	return nil
}

func row(w io.Writer, cells ...any) {
	parts := make([]string, len(cells))
	for i, c := range cells {
		parts[i] = fmt.Sprint(c)
	}
	_, _ = fmt.Fprintln(w, strings.Join(parts, "\t"))
}

func field(w io.Writer, name string, value any) { row(w, name+":", value) }

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func join[T any](items []T) string {
	if len(items) == 0 {
		return "-"
	}
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = fmt.Sprint(it)
	}
	return strings.Join(parts, ",")
}

// parseSize reads a size, with an optional suffix for a power of 1024.
func parseSize(s string) (int64, error) {
	units := map[byte]int64{'K': 1 << 10, 'M': 1 << 20, 'G': 1 << 30, 'T': 1 << 40, 'P': 1 << 50}
	text := strings.ToUpper(strings.TrimSpace(s))
	text = strings.TrimSuffix(strings.TrimSuffix(text, "IB"), "B")
	multiplier := int64(1)
	if text != "" {
		if m, ok := units[text[len(text)-1]]; ok {
			multiplier = m
			text = text[:len(text)-1]
		}
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n <= 0 {
		return 0, usagef("%q isn't a size: use bytes, or a suffix as in 10G", s)
	}
	if n > (1<<63-1)/multiplier {
		return 0, usagef("%q is too large", s)
	}
	return n * multiplier, nil
}

// humanSize shows a size for reading, keeping it exact where that's short.
func humanSize(n int64) string {
	units := []string{"P", "T", "G", "M", "K"}
	sizes := []int64{1 << 50, 1 << 40, 1 << 30, 1 << 20, 1 << 10}
	for i, size := range sizes {
		if n >= size {
			if n%size == 0 {
				return fmt.Sprintf("%d%s", n/size, units[i])
			}
			return fmt.Sprintf("%.1f%s", float64(n)/float64(size), units[i])
		}
	}
	return strconv.FormatInt(n, 10)
}

func atoi(what, s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, usagef("%s must be a number, not %q", what, s)
	}
	return n, nil
}

// repeated collects a flag that can be given several times.
type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

// parseLabels reads repeated key=value flags.
func parseLabels(items []string) (map[string]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	labels := map[string]string{}
	for _, item := range items {
		k, v, ok := strings.Cut(item, "=")
		if !ok || k == "" {
			return nil, usagef("--label takes key=value, not %q", item)
		}
		labels[k] = v
	}
	return labels, nil
}

// readSecret reads a file holding a key. Keys are only taken from files, as other users of the machine can see command lines.
func readSecret(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("key file: %w", err)
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", fmt.Errorf("key file %s is empty", path)
	}
	return secret, nil
}

// wasSet says which flags were given, for updates that should only change those.
func wasSet(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// --- system ---

func (a *app) system(args []string) error {
	if _, err := a.parse(a.flags("system"), args); err != nil {
		return err
	}
	s, err := a.client.System(a.ctx)
	if err != nil {
		return err
	}
	if err := a.emit(s, func(w io.Writer) {
		field(w, "Version", s.Version)
		field(w, "Backend", s.Backend)
		field(w, "Target ready", s.TargetReady)
		field(w, "Kernel", s.Kernel)
		field(w, "Transports", join(s.Capabilities.Transports))
		field(w, "Pool types", join(s.PoolTypes))
		field(w, "Ports", s.Ports)
		field(w, "Hosts", s.Hosts)
		field(w, "Subsystems", s.Subsystems)
		field(w, "Pools", s.Pools)
		field(w, "Volumes", s.Volumes)
		field(w, "Snapshots", s.Snapshots)
	}); err != nil {
		return err
	}
	if a.output != "json" {
		for _, warning := range s.Warnings {
			_, _ = fmt.Fprintln(a.stdout, "Warning: "+warning)
		}
	}
	return nil
}

func (a *app) health(args []string) error {
	if _, err := a.parse(a.flags("health"), args); err != nil {
		return err
	}
	h, err := a.client.Health(a.ctx)
	if err != nil {
		return err
	}
	if err := a.emit(h, func(w io.Writer) { row(w, h.Status) }); err != nil {
		return err
	}
	if h.Status != "ok" {
		return fmt.Errorf("the service reports %q", h.Status)
	}
	return nil
}

// --- ports ---

func portRows(w io.Writer, ports []client.Port) {
	row(w, "ID", "TRANSPORT", "ADDRESS", "SERVICE")
	for _, p := range ports {
		row(w, p.ID, p.Transport, p.Address, p.Service)
	}
}

func (a *app) portList(args []string) error {
	if _, err := a.parse(a.flags("port list"), args); err != nil {
		return err
	}
	ports, err := a.client.ListPorts(a.ctx)
	if err != nil {
		return err
	}
	return a.emit(ports, func(w io.Writer) { portRows(w, ports) })
}

func (a *app) showPort(p *client.Port, err error) error {
	if err != nil {
		return err
	}
	return a.emit(p, func(w io.Writer) { portRows(w, []client.Port{*p}) })
}

func (a *app) portGet(args []string) error {
	pos, err := a.parse(a.flags("port get"), args, "ID")
	if err != nil {
		return err
	}
	id, err := atoi("ID", pos[0])
	if err != nil {
		return err
	}
	return a.showPort(a.client.GetPort(a.ctx, id))
}

func (a *app) portCreate(args []string) error {
	fs := a.flags("port create")
	address := fs.String("address", "", "address to listen on (required)")
	service := fs.Int("service", 0, "TCP port. The API defaults to 4420")
	id := fs.Int("id", 0, "port ID. By default, the lowest free one")
	transport := fs.String("transport", "", "transport. The API defaults to tcp")
	inline := fs.Int("inline-data-size", 0, "inline data size")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	if *address == "" {
		return usagef("port create needs --address")
	}
	set := wasSet(fs)
	in := client.PortCreate{Address: *address, Service: *service, Transport: *transport}
	if set["id"] {
		in.ID = id
	}
	if set["inline-data-size"] {
		in.InlineDataSize = inline
	}
	return a.showPort(a.client.CreatePort(a.ctx, in))
}

func (a *app) portUpdate(args []string) error {
	fs := a.flags("port update")
	address := fs.String("address", "", "address to listen on")
	service := fs.Int("service", 0, "TCP port")
	transport := fs.String("transport", "", "transport")
	inline := fs.Int("inline-data-size", 0, "inline data size")
	pos, err := a.parse(fs, args, "ID")
	if err != nil {
		return err
	}
	id, err := atoi("ID", pos[0])
	if err != nil {
		return err
	}
	set := wasSet(fs)
	var in client.PortUpdate
	if set["address"] {
		in.Address = address
	}
	if set["service"] {
		in.Service = service
	}
	if set["transport"] {
		in.Transport = transport
	}
	if set["inline-data-size"] {
		in.InlineDataSize = inline
	}
	return a.showPort(a.client.UpdatePort(a.ctx, id, in))
}

func (a *app) portDelete(args []string) error {
	pos, err := a.parse(a.flags("port delete"), args, "ID")
	if err != nil {
		return err
	}
	id, err := atoi("ID", pos[0])
	if err != nil {
		return err
	}
	if err := a.client.DeletePort(a.ctx, id); err != nil {
		return err
	}
	return a.done("port %d deleted", id)
}

// --- hosts ---

func hostRows(w io.Writer, hosts []client.Host) {
	row(w, "NQN", "DHCHAP KEY", "CONTROLLER KEY", "HASH", "DH GROUP")
	for _, h := range hosts {
		row(w, h.NQN, h.HasDHCHAPKey, h.HasDHCHAPCtrlKey, dash(h.DHCHAPHash), dash(h.DHCHAPDHGroup))
	}
}

func (a *app) hostList(args []string) error {
	if _, err := a.parse(a.flags("host list"), args); err != nil {
		return err
	}
	hosts, err := a.client.ListHosts(a.ctx)
	if err != nil {
		return err
	}
	return a.emit(hosts, func(w io.Writer) { hostRows(w, hosts) })
}

func (a *app) showHost(h *client.Host, err error) error {
	if err != nil {
		return err
	}
	return a.emit(h, func(w io.Writer) { hostRows(w, []client.Host{*h}) })
}

func (a *app) hostGet(args []string) error {
	pos, err := a.parse(a.flags("host get"), args, "NQN")
	if err != nil {
		return err
	}
	return a.showHost(a.client.GetHost(a.ctx, pos[0]))
}

func dhchapFlags(fs *flag.FlagSet) (key, ctrlKey, hash, group *string) {
	key = fs.String("dhchap-key-file", "", "file holding the host's DH-HMAC-CHAP key")
	ctrlKey = fs.String("dhchap-ctrl-key-file", "", "file holding the controller's key, for authenticating both ways")
	hash = fs.String("dhchap-hash", "", "hash, such as hmac(sha256)")
	group = fs.String("dhchap-dhgroup", "", "Diffie-Hellman group, such as ffdhe2048")
	return
}

func (a *app) hostCreate(args []string) error {
	fs := a.flags("host create")
	keyFile, ctrlFile, hash, group := dhchapFlags(fs)
	pos, err := a.parse(fs, args, "NQN")
	if err != nil {
		return err
	}
	key, err := readSecret(*keyFile)
	if err != nil {
		return err
	}
	ctrl, err := readSecret(*ctrlFile)
	if err != nil {
		return err
	}
	return a.showHost(a.client.CreateHost(a.ctx, client.HostCreate{
		NQN: pos[0], DHCHAPKey: key, DHCHAPCtrlKey: ctrl, DHCHAPHash: *hash, DHCHAPDHGroup: *group,
	}))
}

func (a *app) hostUpdate(args []string) error {
	fs := a.flags("host update")
	keyFile, ctrlFile, hash, group := dhchapFlags(fs)
	pos, err := a.parse(fs, args, "NQN")
	if err != nil {
		return err
	}
	set := wasSet(fs)
	var in client.HostUpdate
	if set["dhchap-key-file"] {
		key, err := readSecret(*keyFile)
		if err != nil {
			return err
		}
		in.DHCHAPKey = &key
	}
	if set["dhchap-ctrl-key-file"] {
		ctrl, err := readSecret(*ctrlFile)
		if err != nil {
			return err
		}
		in.DHCHAPCtrlKey = &ctrl
	}
	if set["dhchap-hash"] {
		in.DHCHAPHash = hash
	}
	if set["dhchap-dhgroup"] {
		in.DHCHAPDHGroup = group
	}
	return a.showHost(a.client.UpdateHost(a.ctx, pos[0], in))
}

func (a *app) hostDelete(args []string) error {
	pos, err := a.parse(a.flags("host delete"), args, "NQN")
	if err != nil {
		return err
	}
	if err := a.client.DeleteHost(a.ctx, pos[0]); err != nil {
		return err
	}
	return a.done("host %s deleted", pos[0])
}

// --- subsystems ---

func subsystemRows(w io.Writer, subs []client.Subsystem) {
	row(w, "NQN", "SERIAL", "ANY HOST", "HOSTS", "PORTS", "NAMESPACES")
	for _, s := range subs {
		row(w, s.NQN, s.Serial, s.AllowAnyHost, join(s.AllowedHosts), join(s.Ports), len(s.Namespaces))
	}
}

func (a *app) subsystemList(args []string) error {
	if _, err := a.parse(a.flags("subsystem list"), args); err != nil {
		return err
	}
	subs, err := a.client.ListSubsystems(a.ctx)
	if err != nil {
		return err
	}
	return a.emit(subs, func(w io.Writer) { subsystemRows(w, subs) })
}

func (a *app) showSubsystem(s *client.Subsystem, err error) error {
	if err != nil {
		return err
	}
	return a.emit(s, func(w io.Writer) { subsystemRows(w, []client.Subsystem{*s}) })
}

func (a *app) subsystemGet(args []string) error {
	pos, err := a.parse(a.flags("subsystem get"), args, "NQN")
	if err != nil {
		return err
	}
	return a.showSubsystem(a.client.GetSubsystem(a.ctx, pos[0]))
}

func (a *app) subsystemCreate(args []string) error {
	fs := a.flags("subsystem create")
	var hosts, ports repeated
	fs.Var(&hosts, "allow-host", "NQN of a host that may connect. Can be repeated")
	fs.Var(&ports, "port", "ID of a port to offer it on. Can be repeated")
	anyHost := fs.Bool("allow-any-host", false, "let any host connect")
	serial := fs.String("serial", "", "serial number. Generated by default")
	model := fs.String("model", "", "model name")
	passthru := fs.String("passthru-device", "", "NVMe controller to pass through, on the kernel target")
	pos, err := a.parse(fs, args, "NQN")
	if err != nil {
		return err
	}
	in := client.SubsystemCreate{NQN: pos[0], Serial: *serial, Model: *model, AllowAnyHost: *anyHost, AllowedHosts: hosts, PassthruDevice: *passthru}
	for _, p := range ports {
		id, err := atoi("--port", p)
		if err != nil {
			return err
		}
		in.Ports = append(in.Ports, id)
	}
	return a.showSubsystem(a.client.CreateSubsystem(a.ctx, in))
}

func (a *app) subsystemUpdate(args []string) error {
	fs := a.flags("subsystem update")
	anyHost := fs.Bool("allow-any-host", false, "let any host connect")
	pos, err := a.parse(fs, args, "NQN")
	if err != nil {
		return err
	}
	var in client.SubsystemUpdate
	if wasSet(fs)["allow-any-host"] {
		in.AllowAnyHost = anyHost
	}
	return a.showSubsystem(a.client.UpdateSubsystem(a.ctx, pos[0], in))
}

func (a *app) subsystemDelete(args []string) error {
	pos, err := a.parse(a.flags("subsystem delete"), args, "NQN")
	if err != nil {
		return err
	}
	if err := a.client.DeleteSubsystem(a.ctx, pos[0]); err != nil {
		return err
	}
	return a.done("subsystem %s deleted", pos[0])
}

func (a *app) subsystemAllow(args []string) error {
	pos, err := a.parse(a.flags("subsystem allow"), args, "NQN", "HOST")
	if err != nil {
		return err
	}
	if err := a.client.AllowHost(a.ctx, pos[0], pos[1]); err != nil {
		return err
	}
	return a.done("%s may connect to %s", pos[1], pos[0])
}

func (a *app) subsystemDeny(args []string) error {
	pos, err := a.parse(a.flags("subsystem deny"), args, "NQN", "HOST")
	if err != nil {
		return err
	}
	if err := a.client.DenyHost(a.ctx, pos[0], pos[1]); err != nil {
		return err
	}
	return a.done("%s can no longer connect to %s", pos[1], pos[0])
}

func (a *app) subsystemAttach(args []string) error {
	pos, err := a.parse(a.flags("subsystem attach"), args, "NQN", "PORT")
	if err != nil {
		return err
	}
	port, err := atoi("PORT", pos[1])
	if err != nil {
		return err
	}
	if err := a.client.AttachPort(a.ctx, pos[0], port); err != nil {
		return err
	}
	return a.done("%s is offered on port %d", pos[0], port)
}

func (a *app) subsystemDetach(args []string) error {
	pos, err := a.parse(a.flags("subsystem detach"), args, "NQN", "PORT")
	if err != nil {
		return err
	}
	port, err := atoi("PORT", pos[1])
	if err != nil {
		return err
	}
	if err := a.client.DetachPort(a.ctx, pos[0], port); err != nil {
		return err
	}
	return a.done("%s is no longer offered on port %d", pos[0], port)
}

// --- namespaces ---

func namespaceRows(w io.Writer, list []client.Namespace) {
	row(w, "NSID", "SIZE", "ENABLED", "LIVE", "VOLUME", "DEVICE", "NGUID")
	for _, n := range list {
		row(w, n.NSID, humanSize(n.SizeBytes), n.Enabled, n.LiveEnabled, dash(n.Volume), n.DevicePath, n.NGUID)
	}
}

func (a *app) namespaceList(args []string) error {
	pos, err := a.parse(a.flags("namespace list"), args, "NQN")
	if err != nil {
		return err
	}
	list, err := a.client.ListNamespaces(a.ctx, pos[0])
	if err != nil {
		return err
	}
	return a.emit(list, func(w io.Writer) { namespaceRows(w, list) })
}

func (a *app) showNamespace(n *client.Namespace, err error) error {
	if err != nil {
		return err
	}
	return a.emit(n, func(w io.Writer) { namespaceRows(w, []client.Namespace{*n}) })
}

func (a *app) namespaceGet(args []string) error {
	pos, err := a.parse(a.flags("namespace get"), args, "NQN", "NSID")
	if err != nil {
		return err
	}
	nsid, err := atoi("NSID", pos[1])
	if err != nil {
		return err
	}
	return a.showNamespace(a.client.GetNamespace(a.ctx, pos[0], nsid))
}

func (a *app) namespaceCreate(args []string) error {
	fs := a.flags("namespace create")
	volume := fs.String("volume", "", "export this volume")
	device := fs.String("device", "", "export this block device or file")
	file := fs.String("file", "", "create a sparse file with this name, and export it. Needs --size")
	size := fs.String("size", "", "size of the new file")
	nsid := fs.Int("nsid", 0, "namespace ID. By default, the lowest free one")
	uuid := fs.String("uuid", "", "UUID. Generated by default")
	nguid := fs.String("nguid", "", "NGUID. Generated by default")
	buffered := fs.Bool("buffered-io", false, "serve through the page cache")
	disabled := fs.Bool("disabled", false, "create it without serving it yet")
	pos, err := a.parse(fs, args, "NQN")
	if err != nil {
		return err
	}
	chosen := 0
	for _, v := range []string{*volume, *device, *file} {
		if v != "" {
			chosen++
		}
	}
	if chosen != 1 {
		return usagef("namespace create needs one of --volume, --device or --file")
	}
	in := client.NamespaceCreate{NSID: *nsid, Volume: *volume, DevicePath: *device, UUID: *uuid, NGUID: *nguid, BufferedIO: *buffered}
	if *file != "" {
		if *size == "" {
			return usagef("--file needs --size")
		}
		bytes, err := parseSize(*size)
		if err != nil {
			return err
		}
		in.File = &client.NamespaceFile{Name: *file, SizeBytes: bytes}
	} else if *size != "" {
		return usagef("--size only goes with --file")
	}
	if *disabled {
		enabled := false
		in.Enabled = &enabled
	}
	return a.showNamespace(a.client.CreateNamespace(a.ctx, pos[0], in))
}

func (a *app) namespaceUpdate(args []string) error {
	fs := a.flags("namespace update")
	enabled := fs.Bool("enabled", true, "whether to serve it")
	device := fs.String("device", "", "change the device")
	buffered := fs.Bool("buffered-io", false, "serve through the page cache")
	size := fs.String("size", "", "grow a file this service created")
	pos, err := a.parse(fs, args, "NQN", "NSID")
	if err != nil {
		return err
	}
	nsid, err := atoi("NSID", pos[1])
	if err != nil {
		return err
	}
	set := wasSet(fs)
	var in client.NamespaceUpdate
	if set["enabled"] {
		in.Enabled = enabled
	}
	if set["device"] {
		in.DevicePath = device
	}
	if set["buffered-io"] {
		in.BufferedIO = buffered
	}
	if set["size"] {
		bytes, err := parseSize(*size)
		if err != nil {
			return err
		}
		in.SizeBytes = &bytes
	}
	return a.showNamespace(a.client.UpdateNamespace(a.ctx, pos[0], nsid, in))
}

func (a *app) namespaceDelete(args []string) error {
	fs := a.flags("namespace delete")
	deleteFile := fs.Bool("delete-file", false, "also delete a file this service created")
	pos, err := a.parse(fs, args, "NQN", "NSID")
	if err != nil {
		return err
	}
	nsid, err := atoi("NSID", pos[1])
	if err != nil {
		return err
	}
	if err := a.client.DeleteNamespace(a.ctx, pos[0], nsid, *deleteFile); err != nil {
		return err
	}
	return a.done("namespace %d of %s deleted", nsid, pos[0])
}

// --- pools ---

func poolRows(w io.Writer, pools []client.Pool) {
	row(w, "NAME", "TYPE", "STATE", "TOTAL", "USED", "VOLUMES", "SNAPSHOTS", "WHERE")
	for _, p := range pools {
		total, used := "-", "-"
		if p.Capacity != nil {
			total = humanSize(p.Capacity.TotalBytes)
			used = fmt.Sprintf("%.0f%%", p.Capacity.UsedPercent)
		}
		where := p.Path
		if p.VolumeGroup != "" {
			where = p.VolumeGroup + "/" + p.ThinPool
		}
		row(w, p.Name, p.Type, p.State, total, used, p.Volumes, p.Snapshots, where)
	}
}

// poolWarnings prints what's wrong with pools below a table, as a pool's trouble shouldn't take looking for.
func (a *app) poolWarnings(pools []client.Pool) {
	if a.output == "json" {
		return
	}
	for _, p := range pools {
		for _, warning := range p.Warnings {
			_, _ = fmt.Fprintf(a.stdout, "Warning: pool %s: %s\n", p.Name, warning)
		}
	}
}

func (a *app) poolList(args []string) error {
	if _, err := a.parse(a.flags("pool list"), args); err != nil {
		return err
	}
	pools, err := a.client.ListPools(a.ctx)
	if err != nil {
		return err
	}
	if err := a.emit(pools, func(w io.Writer) { poolRows(w, pools) }); err != nil {
		return err
	}
	a.poolWarnings(pools)
	return nil
}

func (a *app) showPool(p *client.Pool, err error) error {
	if err != nil {
		return err
	}
	if err := a.emit(p, func(w io.Writer) { poolRows(w, []client.Pool{*p}) }); err != nil {
		return err
	}
	a.poolWarnings([]client.Pool{*p})
	return nil
}

func (a *app) poolGet(args []string) error {
	pos, err := a.parse(a.flags("pool get"), args, "NAME")
	if err != nil {
		return err
	}
	return a.showPool(a.client.GetPool(a.ctx, pos[0]))
}

func (a *app) poolCreate(args []string) error {
	fs := a.flags("pool create")
	kind := fs.String("type", "", "reflink or lvm-thin (required)")
	path := fs.String("path", "", "directory on XFS or btrfs, for reflink")
	vg := fs.String("volume-group", "", "volume group, for lvm-thin")
	thin := fs.String("thin-pool", "", "thin pool in that group, for lvm-thin")
	warning := fs.Int("warning-percent", 0, "warn when it's this full")
	var labels repeated
	fs.Var(&labels, "label", "key=value. Can be repeated")
	pos, err := a.parse(fs, args, "NAME")
	if err != nil {
		return err
	}
	if *kind == "" {
		return usagef("pool create needs --type")
	}
	parsed, err := parseLabels(labels)
	if err != nil {
		return err
	}
	in := client.PoolCreate{Name: pos[0], Type: *kind, Path: *path, VolumeGroup: *vg, ThinPool: *thin, Labels: parsed}
	if wasSet(fs)["warning-percent"] {
		in.WarningPercent = warning
	}
	return a.showPool(a.client.CreatePool(a.ctx, in))
}

func labelUpdate(items []string, clear bool) (*map[string]string, error) {
	if clear && len(items) > 0 {
		return nil, usagef("--clear-labels and --label don't go together")
	}
	if clear {
		return &map[string]string{}, nil
	}
	if len(items) == 0 {
		return nil, nil
	}
	parsed, err := parseLabels(items)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func (a *app) poolUpdate(args []string) error {
	fs := a.flags("pool update")
	warning := fs.Int("warning-percent", 0, "warn when it's this full")
	clear := fs.Bool("clear-labels", false, "remove all labels")
	var labels repeated
	fs.Var(&labels, "label", "key=value, replacing all labels. Can be repeated")
	pos, err := a.parse(fs, args, "NAME")
	if err != nil {
		return err
	}
	var in client.PoolUpdate
	if in.Labels, err = labelUpdate(labels, *clear); err != nil {
		return err
	}
	if wasSet(fs)["warning-percent"] {
		in.WarningPercent = warning
	}
	return a.showPool(a.client.UpdatePool(a.ctx, pos[0], in))
}

func (a *app) poolDelete(args []string) error {
	pos, err := a.parse(a.flags("pool delete"), args, "NAME")
	if err != nil {
		return err
	}
	if err := a.client.DeletePool(a.ctx, pos[0]); err != nil {
		return err
	}
	return a.done("pool %s forgotten, and its storage left alone", pos[0])
}

// --- volumes ---

func volumeRows(w io.Writer, vols []client.Volume) {
	row(w, "NAME", "POOL", "SIZE", "SOURCE", "MAPPED TO", "NGUID")
	for _, v := range vols {
		source, mapped := "-", "-"
		if v.Source != nil {
			source = firstNonEmpty(v.Source.Snapshot, v.Source.Volume)
		}
		if v.MappedTo != nil {
			mapped = fmt.Sprintf("%s/%d", v.MappedTo.Subsystem, v.MappedTo.NSID)
		}
		row(w, v.Name, v.Pool, humanSize(v.SizeBytes), source, mapped, v.NGUID)
	}
}

func (a *app) volumeList(args []string) error {
	fs := a.flags("volume list")
	pool := fs.String("pool", "", "only this pool's")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	vols, err := a.client.ListVolumes(a.ctx, *pool)
	if err != nil {
		return err
	}
	return a.emit(vols, func(w io.Writer) { volumeRows(w, vols) })
}

func (a *app) showVolume(v *client.Volume, err error) error {
	if err != nil {
		return err
	}
	return a.emit(v, func(w io.Writer) { volumeRows(w, []client.Volume{*v}) })
}

func (a *app) volumeGet(args []string) error {
	pos, err := a.parse(a.flags("volume get"), args, "NAME")
	if err != nil {
		return err
	}
	return a.showVolume(a.client.GetVolume(a.ctx, pos[0]))
}

func (a *app) volumeUsage(args []string) error {
	pos, err := a.parse(a.flags("volume usage"), args, "NAME")
	if err != nil {
		return err
	}
	u, err := a.client.GetVolumeUsage(a.ctx, pos[0])
	if err != nil {
		return err
	}
	return a.emit(u, func(w io.Writer) {
		row(w, "SIZE", "USED", "PERCENT")
		row(w, humanSize(u.SizeBytes), humanSize(u.UsedBytes), fmt.Sprintf("%.1f%%", 100*float64(u.UsedBytes)/float64(max(u.SizeBytes, 1))))
	})
}

func (a *app) volumeCreate(args []string) error {
	fs := a.flags("volume create")
	size := fs.String("size", "", "size. A clone defaults to its source's")
	pool := fs.String("pool", "", "which pool, if there's more than one")
	fromSnapshot := fs.String("from-snapshot", "", "clone this snapshot")
	fromVolume := fs.String("from-volume", "", "clone this volume")
	var labels repeated
	fs.Var(&labels, "label", "key=value. Can be repeated")
	pos, err := a.parse(fs, args, "NAME")
	if err != nil {
		return err
	}
	if *fromSnapshot != "" && *fromVolume != "" {
		return usagef("--from-snapshot and --from-volume don't go together")
	}
	parsed, err := parseLabels(labels)
	if err != nil {
		return err
	}
	in := client.VolumeCreate{Name: pos[0], Pool: *pool, Labels: parsed}
	if *fromSnapshot != "" || *fromVolume != "" {
		in.Source = &client.VolumeSource{Snapshot: *fromSnapshot, Volume: *fromVolume}
	}
	switch {
	case *size != "":
		if in.SizeBytes, err = parseSize(*size); err != nil {
			return err
		}
	case in.Source == nil:
		return usagef("volume create needs --size, unless it's a clone")
	}
	return a.showVolume(a.client.CreateVolume(a.ctx, in))
}

func (a *app) volumeUpdate(args []string) error {
	fs := a.flags("volume update")
	size := fs.String("size", "", "grow it to this size")
	clear := fs.Bool("clear-labels", false, "remove all labels")
	var labels repeated
	fs.Var(&labels, "label", "key=value, replacing all labels. Can be repeated")
	pos, err := a.parse(fs, args, "NAME")
	if err != nil {
		return err
	}
	var in client.VolumeUpdate
	if in.Labels, err = labelUpdate(labels, *clear); err != nil {
		return err
	}
	if *size != "" {
		bytes, err := parseSize(*size)
		if err != nil {
			return err
		}
		in.SizeBytes = &bytes
	}
	return a.showVolume(a.client.UpdateVolume(a.ctx, pos[0], in))
}

func (a *app) volumeDelete(args []string) error {
	pos, err := a.parse(a.flags("volume delete"), args, "NAME")
	if err != nil {
		return err
	}
	if err := a.client.DeleteVolume(a.ctx, pos[0]); err != nil {
		return err
	}
	return a.done("volume %s deleted", pos[0])
}

// --- snapshots ---

func snapshotRows(w io.Writer, snaps []client.Snapshot) {
	row(w, "NAME", "VOLUME", "POOL", "SIZE", "CREATED")
	for _, s := range snaps {
		row(w, s.Name, s.Volume, s.Pool, humanSize(s.SizeBytes), s.CreatedAt)
	}
}

func (a *app) snapshotList(args []string) error {
	fs := a.flags("snapshot list")
	volume := fs.String("volume", "", "only this volume's")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	snaps, err := a.client.ListSnapshots(a.ctx, *volume)
	if err != nil {
		return err
	}
	return a.emit(snaps, func(w io.Writer) { snapshotRows(w, snaps) })
}

func (a *app) showSnapshot(s *client.Snapshot, err error) error {
	if err != nil {
		return err
	}
	return a.emit(s, func(w io.Writer) { snapshotRows(w, []client.Snapshot{*s}) })
}

func (a *app) snapshotGet(args []string) error {
	pos, err := a.parse(a.flags("snapshot get"), args, "NAME")
	if err != nil {
		return err
	}
	return a.showSnapshot(a.client.GetSnapshot(a.ctx, pos[0]))
}

func (a *app) snapshotCreate(args []string) error {
	fs := a.flags("snapshot create")
	volume := fs.String("volume", "", "the volume to snapshot (required)")
	pos, err := a.parse(fs, args, "NAME")
	if err != nil {
		return err
	}
	if *volume == "" {
		return usagef("snapshot create needs --volume")
	}
	return a.showSnapshot(a.client.CreateSnapshot(a.ctx, client.SnapshotCreate{Name: pos[0], Volume: *volume}))
}

func (a *app) snapshotDelete(args []string) error {
	pos, err := a.parse(a.flags("snapshot delete"), args, "NAME")
	if err != nil {
		return err
	}
	if err := a.client.DeleteSnapshot(a.ctx, pos[0]); err != nil {
		return err
	}
	return a.done("snapshot %s deleted", pos[0])
}
