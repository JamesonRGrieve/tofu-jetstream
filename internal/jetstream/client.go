// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Package jetstream is a minimal CLI client for TP-Link JetStream smart
// switches (e.g. the TL-SG2008) driven over TELNET.
//
// Why telnet and not SSH or HTTP. The TL-SG2008 (IPSSH-6.6.0) presents only a
// legacy `ssh-dss` host key, which OpenSSH 10 and modern Go x/crypto/ssh have
// removed — so SSH is unusable without re-enabling a deprecated, insecure
// algorithm. The switch has no documented REST API on this firmware. Its telnet
// CLI, however, exposes the full IOS-style configuration grammar
// (`show running-config`, `configure`, `interface …`, `vlan …`,
// `copy running-config startup-config`), which is an exact, structured read of
// any config block — exactly what the manage-declared-only subset model needs to
// compute a 0-diff on import. Telnet is therefore the only workable transport;
// see CLAUDE.md §"Transport".
//
// Go has no telnet in the standard library, so this package implements the
// minimum: NVT IAC option negotiation (we refuse every option), login + enable,
// "Press any key to continue" paging, and command/prompt framing. The protocol
// plumbing (IAC parsing, prompt/paging detection, running-config block parsing,
// command rendering) is factored into pure functions so it is unit-testable
// without a live switch; the networked session layer is a thin shell over them.
package jetstream

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Telnet NVT command bytes (RFC 854 / 855).
const (
	iac  = 255 // Interpret As Command
	dont = 254
	doo  = 253 // "DO" (doo to avoid colliding with the Go keyword)
	wont = 252
	will = 251
	sb   = 250 // subnegotiation begin
	se   = 240 // subnegotiation end
)

// defaultTelnetPort is the standard telnet port.
const defaultTelnetPort = "23"

// defaultUsername is the JetStream admin account.
const defaultUsername = "admin"

// defaultTimeout bounds a single command's full request/response (login,
// `show running-config` with paging, a config batch). The switch is small, so a
// minute is generous headroom, not an expected wait.
const defaultTimeout = 60 * time.Second

// lineEnding terminates every command sent to the CLI. Telnet NVT end-of-line is
// CR LF; the JetStream CLI executes on the CR and swallows the trailing LF.
const lineEnding = "\r\n"

// saveCommand persists the running-config to startup-config (survives reboot).
const saveCommand = "copy running-config startup-config"

// runningConfigCommand dumps the full device configuration.
const runningConfigCommand = "show running-config"

// blockHeaderPrefixes identify a running-config line that opens a configuration
// block (a context with its own indented body): VLAN definitions and interface
// blocks (`interface vlan N`, `interface gigabitEthernet 1/0/X`). Everything
// else at column 0 is a global/standalone command (context "").
var blockHeaderPrefixes = []string{"vlan ", "interface "}

// pagingMarkers are the substrings the CLI emits when output is paused waiting
// for a keypress. Matched case-insensitively; on a match we send a space to
// continue and strip the marker from the captured output.
var pagingMarkers = []string{"press any key to continue", "--more--", "(q to quit)"}

// pagingLineRE strips a residual paging prompt (plus the CR/space/backspace runs
// the CLI uses to erase it) from captured output. It matches only the known
// marker phrases so it never consumes real content that follows on the same line.
var pagingLineRE = regexp.MustCompile(`(?i)[\r\x08 ]*(press any key to continue( ?\(q to quit\))?|\(q to quit\)|--more--)[\r\x08 ]*`)

// promptRE matches a CLI prompt at the tail of the buffer: a device name,
// optionally a `(config…)` mode suffix, ending in `>` (user) or `#` (priv).
// e.g. `TL-SG2008>`, `TL-SG2008#`, `TL-SG2008(config)#`, `TL-SG2008(config-if)#`.
var promptRE = regexp.MustCompile(`^[\w.\-]+(\([\w-]+\))?[>#]$`)

// Client drives a JetStream switch over a single, mutex-serialized telnet
// session. The switch permits very few concurrent telnet sessions, so all
// operations serialize through one connection; it is established lazily, healed
// on error, and the running-config is cached and invalidated on every write.
// Safe for concurrent use; callers may share one Client across resources (the
// provider does).
type Client struct {
	addr     string
	port     string
	user     string
	password string
	timeout  time.Duration
	// dial opens a transport connection; overridable in tests. Defaults to TCP.
	dial func(ctx context.Context) (net.Conn, error)

	mu       sync.Mutex
	conn     net.Conn
	leftover []byte // partial telnet sequence carried across reads

	cachedConfig string
	configValid  bool
}

// Config configures a Client.
type Config struct {
	// Host is the switch address (host or host:port), no scheme.
	Host string
	// Username is the telnet login (default "admin").
	Username string
	// Password is the telnet login password.
	Password string
	// Port is the telnet port (default "23"). A port in Host overrides it.
	Port string
	// Timeout bounds a single command's full request/response (default 60s).
	Timeout time.Duration
}

// NewClient builds a Client. It does not contact the switch until the first
// operation.
func NewClient(c Config) *Client {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	user := strings.TrimSpace(c.Username)
	if user == "" {
		user = defaultUsername
	}
	addr, port := splitHostPort(c.Host)
	if port == "" {
		port = strings.TrimSpace(c.Port)
	}
	if port == "" {
		port = defaultTelnetPort
	}
	cl := &Client{
		addr:     addr,
		port:     port,
		user:     user,
		password: c.Password,
		timeout:  timeout,
	}
	cl.dial = cl.dialTCP
	return cl
}

// splitHostPort splits "host" or "host:port" into (host, port). Port is "" when
// not given. IPv6 literals without brackets are returned unsplit.
func splitHostPort(h string) (string, string) {
	h = strings.TrimSpace(h)
	h = strings.TrimPrefix(h, "telnet://")
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i+1:], "]") {
		if _, err := strconv.Atoi(h[i+1:]); err == nil {
			return h[:i], h[i+1:]
		}
	}
	return h, ""
}

func (c *Client) dialTCP(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{Timeout: c.timeout}
	return d.DialContext(ctx, "tcp", net.JoinHostPort(c.addr, c.port))
}

// ---------------------------------------------------------------------------
// Pure telnet/CLI helpers — unit-tested without a live switch.
// ---------------------------------------------------------------------------

// processTelnet scans a raw telnet byte stream, returning the application data
// with all IAC command sequences removed, the bytes that must be written back
// (our option refusals), and any trailing incomplete sequence to carry into the
// next read. We refuse every option: a DO becomes WONT, a WILL becomes DONT;
// DONT/WONT acknowledgements need no reply; subnegotiations are skipped.
func processTelnet(buf []byte) (clean, reply, rest []byte) {
	i := 0
	for i < len(buf) {
		b := buf[i]
		if b != iac {
			clean = append(clean, b)
			i++
			continue
		}
		// At an IAC. Need at least the command byte.
		if i+1 >= len(buf) {
			rest = buf[i:]
			return clean, reply, rest
		}
		cmd := buf[i+1]
		switch cmd {
		case iac: // escaped 0xFF -> literal byte
			clean = append(clean, iac)
			i += 2
		case will, wont, doo, dont:
			if i+2 >= len(buf) {
				rest = buf[i:]
				return clean, reply, rest
			}
			opt := buf[i+2]
			switch cmd {
			case doo:
				reply = append(reply, iac, wont, opt)
			case will:
				reply = append(reply, iac, dont, opt)
			}
			// DONT / WONT are acknowledgements; replying would loop.
			i += 3
		case sb:
			// Subnegotiation: skip through to IAC SE.
			j := i + 2
			found := false
			for j+1 < len(buf) {
				if buf[j] == iac && buf[j+1] == se {
					found = true
					break
				}
				j++
			}
			if !found {
				rest = buf[i:]
				return clean, reply, rest
			}
			i = j + 2
		default:
			// Other 2-byte commands (NOP, GA, …) — ignore.
			i += 2
		}
	}
	return clean, reply, rest
}

// NormalizeLine trims a CLI line and collapses internal whitespace runs to a
// single space, so the device's rendering and the declared line compare equal
// regardless of indentation/spacing. Case and quoting are preserved.
func NormalizeLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// isBlockHeader reports whether a column-0 running-config line opens a
// configuration block (VLAN or interface).
func isBlockHeader(line string) bool {
	l := strings.ToLower(strings.TrimSpace(line))
	for _, p := range blockHeaderPrefixes {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// parseRunningConfig parses `show running-config` output into a map of context
// -> normalized body lines. The context key is the normalized block header
// (e.g. "interface gigabitEthernet 1/0/6", "vlan 2"); global/standalone commands
// collect under the "" key. It is robust to either indented or flush-left bodies
// because a block runs from its header until the next header or a separator line
// (`#` / `!` / blank).
func parseRunningConfig(raw string) map[string][]string {
	blocks := map[string][]string{}
	ctx := ""
	for _, rawLine := range strings.Split(raw, "\n") {
		line := strings.TrimRight(rawLine, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "#" || trimmed == "!" {
			ctx = "" // separator closes the current block
			continue
		}
		indented := line[0] == ' ' || line[0] == '\t'
		if !indented && isBlockHeader(trimmed) {
			ctx = NormalizeLine(trimmed)
			if _, ok := blocks[ctx]; !ok {
				blocks[ctx] = nil
			}
			continue
		}
		if ctx != "" {
			blocks[ctx] = append(blocks[ctx], NormalizeLine(trimmed))
			continue
		}
		blocks[""] = append(blocks[""], NormalizeLine(trimmed))
	}
	return blocks
}

// blockLines returns the normalized body lines for a context and whether the
// block is present at all. An empty context returns the global lines.
func blockLines(cfg map[string][]string, ctx string) ([]string, bool) {
	key := NormalizeLine(ctx)
	lines, ok := cfg[key]
	return lines, ok
}

// isPagingPrompt reports whether the captured buffer currently ends in a paging
// pause waiting for a keypress.
func isPagingPrompt(s string) bool {
	low := strings.ToLower(s)
	for _, m := range pagingMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// stripPagingMarkers removes residual paging prompts from captured output.
func stripPagingMarkers(b []byte) []byte {
	return pagingLineRE.ReplaceAll(b, nil)
}

// endsWithPrompt reports whether the buffer ends with a CLI prompt (the marker
// that a command has finished and the device is ready for the next).
func endsWithPrompt(buf []byte) bool {
	s := strings.TrimRight(string(buf), " \t\r\n")
	if s == "" {
		return false
	}
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return promptRE.MatchString(strings.TrimSpace(s))
}

// cleanOutput strips a command echo (first line) and the trailing prompt (last
// line) from captured output and removes paging artifacts, leaving the body.
func cleanOutput(raw, cmd string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	raw = string(stripPagingMarkers([]byte(raw)))
	lines := strings.Split(raw, "\n")
	// Drop the leading echo of the command (allowing leading blanks).
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	if start < len(lines) && strings.TrimSpace(lines[start]) == strings.TrimSpace(cmd) {
		start++
	}
	// Drop the trailing prompt (and any trailing blanks).
	end := len(lines)
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	if end > start && promptRE.MatchString(strings.TrimSpace(lines[end-1])) {
		end--
	}
	if start >= end {
		return ""
	}
	return strings.Join(lines[start:end], "\n")
}

// applyCommands renders the CLI command sequence that applies lines within a
// context: enter config, (enter the context,) type each line, exit to priv-exec.
func applyCommands(ctx string, lines []string) []string {
	cmds := []string{"configure"}
	if strings.TrimSpace(ctx) != "" {
		cmds = append(cmds, NormalizeLine(ctx))
	}
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			cmds = append(cmds, strings.TrimSpace(l))
		}
	}
	return append(cmds, "end")
}

// removeCommands renders the CLI sequence that negates lines within a context.
func removeCommands(ctx string, lines []string) []string {
	cmds := []string{"configure"}
	if strings.TrimSpace(ctx) != "" {
		cmds = append(cmds, NormalizeLine(ctx))
	}
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			cmds = append(cmds, negate(l))
		}
	}
	return append(cmds, "end")
}

// negate turns a config line into its `no` form (or strips an existing `no`).
func negate(line string) string {
	l := strings.TrimSpace(line)
	if rest := strings.TrimPrefix(l, "no "); rest != l {
		return rest
	}
	return "no " + l
}

// ---------------------------------------------------------------------------
// Networked session layer — a thin shell over the helpers above.
// ---------------------------------------------------------------------------

// ensureSession connects and logs in if not already connected. Caller holds c.mu.
func (c *Client) ensureSession(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return fmt.Errorf("jetstream: telnet dial %s: %w", net.JoinHostPort(c.addr, c.port), err)
	}
	c.conn = conn
	c.leftover = nil
	if err := c.login(ctx); err != nil {
		c.dropLocked()
		return err
	}
	return nil
}

// dropLocked tears down a (possibly broken) session so the next op reconnects.
// Caller holds c.mu.
func (c *Client) dropLocked() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.leftover = nil
}

// login walks User:/Password: prompts to user-exec, then `enable` to priv-exec.
// Caller holds c.mu.
func (c *Client) login(ctx context.Context) error {
	if _, err := c.readUntil(ctx, func(s string) bool {
		l := strings.ToLower(s)
		return strings.Contains(l, "user:") || strings.Contains(l, "username:") || strings.Contains(l, "login:")
	}); err != nil {
		return fmt.Errorf("jetstream login: waiting for username prompt: %w", err)
	}
	if err := c.writeLine(c.user); err != nil {
		return err
	}
	if _, err := c.readUntil(ctx, func(s string) bool {
		return strings.Contains(strings.ToLower(s), "password")
	}); err != nil {
		return fmt.Errorf("jetstream login: waiting for password prompt: %w", err)
	}
	if err := c.writeLine(c.password); err != nil {
		return err
	}
	if _, err := c.readUntil(ctx, func(s string) bool { return endsWithPrompt([]byte(s)) }); err != nil {
		return fmt.Errorf("jetstream login: waiting for shell prompt: %w", err)
	}
	// enable -> priv-exec (#). No enable password on this switch.
	out, err := c.command(ctx, "enable")
	if err != nil {
		return fmt.Errorf("jetstream login: enable: %w", err)
	}
	if strings.Contains(strings.ToLower(out), "password") {
		return fmt.Errorf("jetstream login: enable unexpectedly demanded a password")
	}
	return nil
}

// command sends one command and returns its cleaned output. Caller holds c.mu.
func (c *Client) command(ctx context.Context, cmd string) (string, error) {
	if err := c.writeLine(cmd); err != nil {
		return "", err
	}
	raw, err := c.readUntil(ctx, func(s string) bool { return endsWithPrompt([]byte(s)) })
	if err != nil {
		return "", fmt.Errorf("jetstream command %q: %w", cmd, err)
	}
	return cleanOutput(raw, cmd), nil
}

// writeLine sends a command line terminated by the telnet end-of-line. Caller
// holds c.mu.
func (c *Client) writeLine(s string) error {
	if c.conn == nil {
		return fmt.Errorf("jetstream: not connected")
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(c.timeout))
	if _, err := c.conn.Write([]byte(s + lineEnding)); err != nil {
		c.dropLocked()
		return fmt.Errorf("jetstream: write: %w", err)
	}
	return nil
}

// readUntil reads (handling telnet negotiation and paging) until match returns
// true on the accumulated, telnet-cleaned text. Caller holds c.mu.
func (c *Client) readUntil(ctx context.Context, match func(s string) bool) (string, error) {
	var acc []byte
	deadline := time.Now().Add(c.timeout)
	tmp := make([]byte, 4096)
	for {
		if err := ctx.Err(); err != nil {
			return string(acc), err
		}
		_ = c.conn.SetReadDeadline(deadline)
		n, err := c.conn.Read(tmp)
		if n > 0 {
			data := append(c.leftover, tmp[:n]...)
			clean, reply, rest := processTelnet(data)
			c.leftover = append([]byte(nil), rest...)
			if len(reply) > 0 {
				_ = c.conn.SetWriteDeadline(time.Now().Add(c.timeout))
				if _, werr := c.conn.Write(reply); werr != nil {
					c.dropLocked()
					return string(acc), fmt.Errorf("jetstream: telnet negotiation write: %w", werr)
				}
			}
			acc = append(acc, clean...)
			s := string(acc)
			if match(s) {
				return s, nil
			}
			if isPagingPrompt(s) {
				if _, werr := c.conn.Write([]byte(" ")); werr != nil {
					c.dropLocked()
					return string(acc), fmt.Errorf("jetstream: paging continue write: %w", werr)
				}
				acc = stripPagingMarkers(acc)
				continue
			}
		}
		if err != nil {
			c.dropLocked()
			return string(acc), fmt.Errorf("jetstream: read: %w", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Public operations.
// ---------------------------------------------------------------------------

// RunningConfig returns the body of `show running-config`, cached until the next
// write invalidates it.
func (c *Client) RunningConfig(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runningConfigLocked(ctx)
}

func (c *Client) runningConfigLocked(ctx context.Context) (string, error) {
	if c.configValid {
		return c.cachedConfig, nil
	}
	if err := c.ensureSession(ctx); err != nil {
		return "", err
	}
	out, err := c.command(ctx, runningConfigCommand)
	if err != nil {
		return "", err
	}
	c.cachedConfig = out
	c.configValid = true
	return out, nil
}

// Block returns the normalized body lines of a configuration block (the context,
// e.g. "vlan 2" or "interface gigabitEthernet 1/0/6") and whether it exists.
func (c *Client) Block(ctx context.Context, configContext string) ([]string, bool, error) {
	cfg, err := c.RunningConfig(ctx)
	if err != nil {
		return nil, false, err
	}
	lines, present := blockLines(parseRunningConfig(cfg), configContext)
	return lines, present, nil
}

// ApplyLines applies the declared lines within the context, then saves. The
// config cache is invalidated.
func (c *Client) ApplyLines(ctx context.Context, configContext string, lines []string) error {
	return c.runConfig(ctx, applyCommands(configContext, lines))
}

// RemoveLines negates the declared lines within the context, then saves.
func (c *Client) RemoveLines(ctx context.Context, configContext string, lines []string) error {
	return c.runConfig(ctx, removeCommands(configContext, lines))
}

// Save persists the running-config to startup-config.
func (c *Client) Save(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureSession(ctx); err != nil {
		return err
	}
	_, err := c.command(ctx, saveCommand)
	return err
}

// runConfig runs a config-mode command batch followed by a save, under the lock,
// invalidating the running-config cache.
func (c *Client) runConfig(ctx context.Context, cmds []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureSession(ctx); err != nil {
		return err
	}
	c.configValid = false
	for _, cmd := range cmds {
		out, err := c.command(ctx, cmd)
		if err != nil {
			return err
		}
		if isCLIError(out) {
			return fmt.Errorf("jetstream config %q: %s", cmd, strings.TrimSpace(out))
		}
	}
	if _, err := c.command(ctx, saveCommand); err != nil {
		return err
	}
	return nil
}

// cliErrorMarkers are substrings the CLI emits to signal a rejected command.
var cliErrorMarkers = []string{"bad command", "invalid", "incomplete", "unknown command", "% "}

// isCLIError reports whether command output indicates a rejected command.
func isCLIError(out string) bool {
	low := strings.ToLower(out)
	for _, m := range cliErrorMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// Close tears down the telnet session. Best-effort.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked()
}
