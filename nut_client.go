package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// nutClient reads variables from a NUT upsd server (network protocol, RFC 9271).
// upsd serves reads without a login. Each call opens its own connection, so an
// upsd restart needs no reconnect logic.
type nutClient struct {
	addr    string // host:port
	timeout time.Duration
}

// errNUTVarNotSupported is upsd's reply for a variable the UPS doesn't report.
var errNUTVarNotSupported = errors.New("VAR-NOT-SUPPORTED")

type nutConn struct {
	conn net.Conn
	r    *bufio.Reader
}

func (c nutClient) session(fn func(*nutConn) error) error {
	conn, err := net.DialTimeout("tcp", c.addr, c.timeout)
	if err != nil {
		return fmt.Errorf("connecting to upsd at %s: %w", c.addr, err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return err
	}

	err = fn(&nutConn{conn: conn, r: bufio.NewReader(conn)})
	_, _ = fmt.Fprint(conn, "LOGOUT\n") // best effort; the connection closes either way
	return err
}

// command sends one line and returns the first reply line.
func (s *nutConn) command(line string) (string, error) {
	if _, err := fmt.Fprintf(s.conn, "%s\n", line); err != nil {
		return "", err
	}
	return s.readLine()
}

func (s *nutConn) readLine() (string, error) {
	line, err := s.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if code, ok := strings.CutPrefix(line, "ERR "); ok {
		code, _, _ = strings.Cut(code, " ")
		if code == errNUTVarNotSupported.Error() {
			return "", errNUTVarNotSupported
		}
		return "", fmt.Errorf("upsd error: %s", code)
	}
	return line, nil
}

// listUPS returns the UPS names upsd serves.
func (c nutClient) listUPS() ([]string, error) {
	var names []string
	err := c.session(func(s *nutConn) error {
		line, err := s.command("LIST UPS")
		if err != nil {
			return err
		}
		if line != "BEGIN LIST UPS" {
			return fmt.Errorf("unexpected upsd reply %q", line)
		}
		for {
			line, err := s.readLine()
			if err != nil {
				return err
			}
			if line == "END LIST UPS" {
				return nil
			}
			fields := strings.Fields(line) // UPS <name> "<description>"
			if len(fields) >= 2 && fields[0] == "UPS" {
				names = append(names, fields[1])
			}
		}
	})
	return names, err
}

// getVars returns the requested variables, leaving out any the UPS doesn't report.
func (c nutClient) getVars(ups string, names ...string) (map[string]string, error) {
	vars := make(map[string]string, len(names))
	err := c.session(func(s *nutConn) error {
		for _, name := range names {
			line, err := s.command(fmt.Sprintf("GET VAR %s %s", ups, name))
			if errors.Is(err, errNUTVarNotSupported) {
				continue
			}
			if err != nil {
				return fmt.Errorf("reading %s from UPS %s: %w", name, ups, err)
			}
			prefix := fmt.Sprintf("VAR %s %s ", ups, name)
			quoted, ok := strings.CutPrefix(line, prefix)
			if !ok {
				return fmt.Errorf("unexpected upsd reply %q", line)
			}
			value, err := strconv.Unquote(quoted)
			if err != nil {
				return fmt.Errorf("unexpected upsd reply %q: %w", line, err)
			}
			vars[name] = value
		}
		return nil
	})
	return vars, err
}
