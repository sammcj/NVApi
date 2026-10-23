package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeUPSD is a minimal upsd for tests, serving vars for each UPS.
type fakeUPSD struct {
	mu  sync.Mutex
	ups map[string]map[string]string
}

func (f *fakeUPSD) set(ups, name, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ups[ups][name] = value
}

func startFakeUPSD(t *testing.T, f *fakeUPSD) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return ln.Addr().String()
}

func (f *fakeUPSD) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		reply := func(format string, a ...any) { _, _ = fmt.Fprintf(conn, format+"\n", a...) }
		f.mu.Lock()
		switch {
		case len(fields) == 1 && fields[0] == "LOGOUT":
			reply("OK Goodbye")
			f.mu.Unlock()
			return
		case len(fields) == 2 && fields[0] == "LIST" && fields[1] == "UPS":
			reply("BEGIN LIST UPS")
			for name := range f.ups {
				reply(`UPS %s "Test UPS"`, name)
			}
			reply("END LIST UPS")
		case len(fields) == 4 && fields[0] == "GET" && fields[1] == "VAR":
			vars, ok := f.ups[fields[2]]
			if !ok {
				reply("ERR UNKNOWN-UPS")
				break
			}
			if v, ok := vars[fields[3]]; ok {
				reply(`VAR %s %s "%s"`, fields[2], fields[3], v)
			} else {
				reply("ERR VAR-NOT-SUPPORTED")
			}
		default:
			reply("ERR UNKNOWN-COMMAND")
		}
		f.mu.Unlock()
	}
}

func newFakeUPSD(t *testing.T, vars map[string]string) (*fakeUPSD, nutClient) {
	f := &fakeUPSD{ups: map[string]map[string]string{"eaton": vars}}
	addr := startFakeUPSD(t, f)
	return f, nutClient{addr: addr, timeout: 2 * time.Second}
}

func TestNUTClientGetVars(t *testing.T) {
	_, c := newFakeUPSD(t, map[string]string{"ups.realpower": "137", "ups.status": "OL CHRG"})

	vars, err := c.getVars("eaton", "ups.realpower", "ups.status", "ups.load")
	if err != nil {
		t.Fatal(err)
	}
	if vars["ups.realpower"] != "137" || vars["ups.status"] != "OL CHRG" {
		t.Errorf("unexpected vars %v", vars)
	}
	if _, ok := vars["ups.load"]; ok {
		t.Errorf("unsupported var should be left out, got %v", vars)
	}
}

func TestNUTClientUnknownUPS(t *testing.T) {
	_, c := newFakeUPSD(t, map[string]string{})
	if _, err := c.getVars("missing", "ups.realpower"); err == nil || !strings.Contains(err.Error(), "UNKNOWN-UPS") {
		t.Errorf("expected UNKNOWN-UPS error, got %v", err)
	}
}

func TestNUTClientListUPS(t *testing.T) {
	_, c := newFakeUPSD(t, map[string]string{})
	names, err := c.listUPS()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "eaton" {
		t.Errorf("listUPS() = %v, want [eaton]", names)
	}
}

func TestNUTClientConnectError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	c := nutClient{addr: addr, timeout: time.Second}
	if _, err := c.getVars("eaton", "ups.realpower"); err == nil {
		t.Error("expected a connection error")
	}
}
