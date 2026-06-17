// SPDX-License-Identifier: AGPL-3.0-or-later

package jetstream

import (
	"bufio"
	"context"
	"net"
	"reflect"
	"testing"
	"time"
)

func TestSplitHostPort(t *testing.T) {
	cases := []struct {
		in         string
		host, port string
	}{
		{"192.168.2.182", "192.168.2.182", ""},
		{"192.168.2.182:2323", "192.168.2.182", "2323"},
		{"telnet://switch:23", "switch", "23"},
		{" switch ", "switch", ""},
		{"switch:notaport", "switch:notaport", ""},
	}
	for _, tc := range cases {
		h, p := splitHostPort(tc.in)
		if h != tc.host || p != tc.port {
			t.Errorf("splitHostPort(%q) = (%q,%q), want (%q,%q)", tc.in, h, p, tc.host, tc.port)
		}
	}
}

func TestNewClientDefaults(t *testing.T) {
	c := NewClient(Config{Host: "192.168.2.182"})
	if c.user != defaultUsername {
		t.Errorf("default user = %q, want %q", c.user, defaultUsername)
	}
	if c.port != defaultTelnetPort {
		t.Errorf("default port = %q, want %q", c.port, defaultTelnetPort)
	}
	if c.timeout != defaultTimeout {
		t.Errorf("default timeout = %v, want %v", c.timeout, defaultTimeout)
	}
	// A port in Host overrides the Port field.
	c2 := NewClient(Config{Host: "switch:2323", Port: "23"})
	if c2.port != "2323" {
		t.Errorf("host port should win: got %q, want 2323", c2.port)
	}
	// The Port field is used when Host carries no port.
	c3 := NewClient(Config{Host: "switch", Port: "2323"})
	if c3.port != "2323" {
		t.Errorf("config port should apply: got %q, want 2323", c3.port)
	}
}

func TestProcessTelnet(t *testing.T) {
	cases := []struct {
		name        string
		in          []byte
		wantClean   []byte
		wantReply   []byte
		wantRestLen int
	}{
		{
			name:      "plain data passes through",
			in:        []byte("hello"),
			wantClean: []byte("hello"),
		},
		{
			name:      "DO option is refused with WONT",
			in:        []byte{'a', iac, doo, 1, 'b'},
			wantClean: []byte("ab"),
			wantReply: []byte{iac, wont, 1},
		},
		{
			name:      "WILL option is refused with DONT",
			in:        []byte{iac, will, 3, 'x'},
			wantClean: []byte("x"),
			wantReply: []byte{iac, dont, 3},
		},
		{
			name:      "DONT/WONT acknowledgements get no reply",
			in:        []byte{iac, dont, 1, iac, wont, 3, 'y'},
			wantClean: []byte("y"),
			wantReply: nil,
		},
		{
			name:      "escaped IAC becomes a literal 0xFF",
			in:        []byte{'a', iac, iac, 'b'},
			wantClean: []byte{'a', iac, 'b'},
		},
		{
			name:      "subnegotiation is skipped",
			in:        append(append([]byte{'a', iac, sb, 24, 0}, 't', 'e', 'r', 'm'), iac, se, 'b'),
			wantClean: []byte("ab"),
		},
		{
			name:        "partial DO sequence is carried over as rest",
			in:          []byte{'a', iac, doo},
			wantClean:   []byte("a"),
			wantRestLen: 2,
		},
		{
			name:        "lone trailing IAC is carried over as rest",
			in:          []byte{'a', iac},
			wantClean:   []byte("a"),
			wantRestLen: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clean, reply, rest := processTelnet(tc.in)
			if !reflect.DeepEqual(clean, tc.wantClean) {
				t.Errorf("clean = %v, want %v", clean, tc.wantClean)
			}
			if !reflect.DeepEqual(reply, tc.wantReply) {
				t.Errorf("reply = %v, want %v", reply, tc.wantReply)
			}
			if len(rest) != tc.wantRestLen {
				t.Errorf("rest len = %d, want %d", len(rest), tc.wantRestLen)
			}
		})
	}
}

func TestProcessTelnetSplitSequenceReassembles(t *testing.T) {
	// A DO option split across two reads: the first read carries the partial
	// sequence as rest; prepending it to the next read completes the negotiation.
	clean1, reply1, rest1 := processTelnet([]byte{'a', iac, doo})
	if string(clean1) != "a" || len(reply1) != 0 || len(rest1) != 2 {
		t.Fatalf("first half: clean=%q reply=%v rest=%v", clean1, reply1, rest1)
	}
	clean2, reply2, rest2 := processTelnet(append(rest1, 1, 'b'))
	if string(clean2) != "b" {
		t.Errorf("second half clean = %q, want b", clean2)
	}
	if !reflect.DeepEqual(reply2, []byte{iac, wont, 1}) {
		t.Errorf("second half reply = %v, want WONT 1", reply2)
	}
	if len(rest2) != 0 {
		t.Errorf("second half rest = %v, want empty", rest2)
	}
}

func TestNormalizeLine(t *testing.T) {
	cases := map[string]string{
		"  switchport   pvid   3 ":         "switchport pvid 3",
		"name \"TELUS_WAN\"":               "name \"TELUS_WAN\"",
		"\tip address 192.168.1.253 /24\t": "ip address 192.168.1.253 /24",
		"":                                 "",
	}
	for in, want := range cases {
		if got := NormalizeLine(in); got != want {
			t.Errorf("NormalizeLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsBlockHeader(t *testing.T) {
	headers := []string{"vlan 2", "VLAN 3", "interface vlan 1", "interface gigabitEthernet 1/0/6"}
	for _, h := range headers {
		if !isBlockHeader(h) {
			t.Errorf("isBlockHeader(%q) = false, want true", h)
		}
	}
	body := []string{"name \"x\"", "switchport pvid 3", "ip address 1.2.3.4 255.255.255.0", "snmp-server"}
	for _, b := range body {
		if isBlockHeader(b) {
			t.Errorf("isBlockHeader(%q) = true, want false", b)
		}
	}
}

const sampleRunningConfigFlushLeft = `#
vlan 2
name "TELUS_WAN"
#
vlan 3
name "WAN_STARLINK"
#
interface vlan 1
ip address 192.168.1.253 255.255.255.0
ipv6 enable
#
interface vlan 2
ip address-alloc dhcp
description "WAN_TELUS"
no ipv6 enable
#
interface gigabitEthernet 1/0/6
switchport general allowed vlan 2-3 tagged
#
interface gigabitEthernet 1/0/8
switchport general allowed vlan 3 untagged
switchport pvid 3
#
snmp-server
no controller cloud-based
`

const sampleRunningConfigIndented = `!
vlan 2
 name "TELUS_WAN"
!
interface gigabitEthernet 1/0/6
 switchport general allowed vlan 2-3 tagged
!
`

func TestParseRunningConfigFlushLeft(t *testing.T) {
	cfg := parseRunningConfig(sampleRunningConfigFlushLeft)
	wantBlocks := map[string][]string{
		"vlan 2":                          {`name "TELUS_WAN"`},
		"vlan 3":                          {`name "WAN_STARLINK"`},
		"interface vlan 1":                {"ip address 192.168.1.253 255.255.255.0", "ipv6 enable"},
		"interface vlan 2":                {"ip address-alloc dhcp", `description "WAN_TELUS"`, "no ipv6 enable"},
		"interface gigabitEthernet 1/0/6": {"switchport general allowed vlan 2-3 tagged"},
		"interface gigabitEthernet 1/0/8": {"switchport general allowed vlan 3 untagged", "switchport pvid 3"},
	}
	for ctx, want := range wantBlocks {
		got, ok := cfg[ctx]
		if !ok {
			t.Errorf("missing block %q", ctx)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("block %q = %v, want %v", ctx, got, want)
		}
	}
	// Global standalone commands collect under "".
	wantGlobal := []string{"snmp-server", "no controller cloud-based"}
	if !reflect.DeepEqual(cfg[""], wantGlobal) {
		t.Errorf("global block = %v, want %v", cfg[""], wantGlobal)
	}
}

func TestParseRunningConfigIndented(t *testing.T) {
	cfg := parseRunningConfig(sampleRunningConfigIndented)
	if !reflect.DeepEqual(cfg["vlan 2"], []string{`name "TELUS_WAN"`}) {
		t.Errorf("vlan 2 = %v", cfg["vlan 2"])
	}
	if !reflect.DeepEqual(cfg["interface gigabitEthernet 1/0/6"], []string{"switchport general allowed vlan 2-3 tagged"}) {
		t.Errorf("if 1/0/6 = %v", cfg["interface gigabitEthernet 1/0/6"])
	}
}

func TestBlockLines(t *testing.T) {
	cfg := parseRunningConfig(sampleRunningConfigFlushLeft)
	lines, present := blockLines(cfg, " interface  gigabitEthernet  1/0/6 ") // normalization-insensitive lookup
	if !present {
		t.Fatal("expected block present")
	}
	if !reflect.DeepEqual(lines, []string{"switchport general allowed vlan 2-3 tagged"}) {
		t.Errorf("lines = %v", lines)
	}
	if _, present := blockLines(cfg, "vlan 999"); present {
		t.Error("vlan 999 should be absent")
	}
}

func TestEndsWithPrompt(t *testing.T) {
	prompts := []string{
		"TL-SG2008>",
		"TL-SG2008#",
		"TL-SG2008(config)#",
		"TL-SG2008(config-if)#",
		"some output\r\nTL-SG2008#",
		"output\nTL-SG2008(config-vlan)# ",
	}
	for _, p := range prompts {
		if !endsWithPrompt([]byte(p)) {
			t.Errorf("endsWithPrompt(%q) = false, want true", p)
		}
	}
	notPrompts := []string{
		"",
		"#",                  // bare separator, not a prompt
		"name \"TELUS_WAN\"", // config body
		"User:",              // login prompt handled separately
		"switchport pvid 3",
	}
	for _, p := range notPrompts {
		if endsWithPrompt([]byte(p)) {
			t.Errorf("endsWithPrompt(%q) = true, want false", p)
		}
	}
}

func TestIsPagingPrompt(t *testing.T) {
	if !isPagingPrompt("vlan 2\r\nPress any key to continue (Q to quit)") {
		t.Error("should detect paging marker")
	}
	if !isPagingPrompt("foo --More-- ") {
		t.Error("should detect --More--")
	}
	if isPagingPrompt("normal output\r\nTL-SG2008#") {
		t.Error("should not flag normal output")
	}
}

func TestCleanOutput(t *testing.T) {
	raw := "show running-config\r\nvlan 2\r\n name \"TELUS_WAN\"\r\nTL-SG2008#"
	got := cleanOutput(raw, "show running-config")
	want := "vlan 2\n name \"TELUS_WAN\""
	if got != want {
		t.Errorf("cleanOutput = %q, want %q", got, want)
	}
	// Paging residue is stripped.
	raw2 := "enable\r\nPress any key to continue (Q to quit)body\r\nTL-SG2008#"
	if got := cleanOutput(raw2, "enable"); got != "body" {
		t.Errorf("cleanOutput with paging = %q, want %q", got, "body")
	}
}

func TestApplyCommands(t *testing.T) {
	got := applyCommands("interface gigabitEthernet 1/0/6", []string{"switchport general allowed vlan 2-3 tagged", ""})
	want := []string{"configure", "interface gigabitEthernet 1/0/6", "switchport general allowed vlan 2-3 tagged", "end"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("applyCommands = %v, want %v", got, want)
	}
	// Empty context -> global config, no context line.
	gotGlobal := applyCommands("", []string{"no controller cloud-based"})
	wantGlobal := []string{"configure", "no controller cloud-based", "end"}
	if !reflect.DeepEqual(gotGlobal, wantGlobal) {
		t.Errorf("applyCommands(global) = %v, want %v", gotGlobal, wantGlobal)
	}
}

func TestRemoveCommands(t *testing.T) {
	got := removeCommands("vlan 7", []string{`name "TEMP"`, "no ipv6 enable"})
	want := []string{"configure", "vlan 7", `no name "TEMP"`, "ipv6 enable", "end"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("removeCommands = %v, want %v", got, want)
	}
}

func TestNegate(t *testing.T) {
	cases := map[string]string{
		"ipv6 enable":         "no ipv6 enable",
		"no ipv6 enable":      "ipv6 enable",
		`name "X"`:            `no name "X"`,
		" switchport pvid 3 ": "no switchport pvid 3",
	}
	for in, want := range cases {
		if got := negate(in); got != want {
			t.Errorf("negate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsCLIError(t *testing.T) {
	bad := []string{"Bad command", "% Invalid input", "incomplete command", "Unknown command"}
	for _, b := range bad {
		if !isCLIError(b) {
			t.Errorf("isCLIError(%q) = false, want true", b)
		}
	}
	if isCLIError("vlan 2\n name \"x\"") {
		t.Error("clean output flagged as error")
	}
}

// ---------------------------------------------------------------------------
// Integration: drive the full login + paged read against a scripted fake switch
// over a real localhost TCP socket (buffered, so telnet negotiation replies do
// not deadlock as they would on net.Pipe). This exercises the stateful telnet
// reader, login flow, enable, and paging end to end.
// ---------------------------------------------------------------------------

func newFakeSwitch(t *testing.T, script func(c net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		script(conn)
	}()
	return ln.Addr().String()
}

func TestClientLoginAndRunningConfig(t *testing.T) {
	addr := newFakeSwitch(t, func(c net.Conn) {
		r := bufio.NewReader(c)
		readLine := func() { _, _ = r.ReadBytes('\n') }
		// Initial telnet negotiation + banner + username prompt.
		_, _ = c.Write([]byte{iac, doo, 1, iac, will, 3})
		_, _ = c.Write([]byte("\r\nUser:"))
		readLine() // username (preceded by the client's negotiation replies)
		_, _ = c.Write([]byte("\r\nPassword:"))
		readLine() // password
		_, _ = c.Write([]byte("\r\nTL-SG2008>"))
		readLine() // enable
		_, _ = c.Write([]byte("enable\r\nTL-SG2008#"))
		readLine() // show running-config
		_, _ = c.Write([]byte("show running-config\r\n#\r\nvlan 2\r\n name \"TELUS_WAN\"\r\n"))
		// Pause for paging; the client must send a space to continue.
		_, _ = c.Write([]byte("Press any key to continue (Q to quit)"))
		b, _ := r.ReadByte()
		if b != ' ' {
			t.Errorf("paging continue byte = %q, want space", b)
		}
		_, _ = c.Write([]byte("\r\n#\r\ninterface vlan 1\r\n ip address 192.168.1.253 255.255.255.0\r\nTL-SG2008#"))
	})

	c := NewClient(Config{Host: addr, Username: "admin", Password: "secret", Timeout: 5 * time.Second})
	t.Cleanup(c.Close)

	cfg, err := c.RunningConfig(context.Background())
	if err != nil {
		t.Fatalf("RunningConfig: %v", err)
	}
	blocks := parseRunningConfig(cfg)
	if !reflect.DeepEqual(blocks["vlan 2"], []string{`name "TELUS_WAN"`}) {
		t.Errorf("vlan 2 block = %v", blocks["vlan 2"])
	}
	if !reflect.DeepEqual(blocks["interface vlan 1"], []string{"ip address 192.168.1.253 255.255.255.0"}) {
		t.Errorf("interface vlan 1 block = %v", blocks["interface vlan 1"])
	}

	// Second call is served from cache (the fake only scripts one show).
	if _, err := c.RunningConfig(context.Background()); err != nil {
		t.Fatalf("cached RunningConfig: %v", err)
	}
}
