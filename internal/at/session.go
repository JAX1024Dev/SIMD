package at

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
)

var ErrClosed = errors.New("AT session closed")
var ErrProtocol = errors.New("AT command rejected")

type Session struct {
	port    io.ReadWriteCloser
	gate    chan struct{}
	mu      sync.Mutex
	active  chan string
	command string
	done    chan struct{}
	once    sync.Once
	notify  chan struct{}
}

func New(port io.ReadWriteCloser) *Session {
	s := &Session{port: port, gate: make(chan struct{}, 1), done: make(chan struct{}), notify: make(chan struct{}, 1)}
	go s.read()
	return s
}
func (s *Session) Done() <-chan struct{} { return s.done }

// Notifications coalesce: the consumer always scans the complete store.
func (s *Session) Notifications() <-chan struct{} { return s.notify }
func (s *Session) Close() error {
	var e error
	s.once.Do(func() { close(s.done); e = s.port.Close() })
	return e
}
func (s *Session) emit(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if line == "RDY" || line == "+MATREADY" {
		s.Close()
		return
	}
	if strings.HasPrefix(line, "+CMTI:") || strings.HasPrefix(line, "+CDSI:") || line == "SMS Ready" {
		select {
		case s.notify <- struct{}{}:
		default:
		}
		return
	}
	s.mu.Lock()
	ch := s.active
	command := s.command
	s.mu.Unlock()
	// A URC must never be interpreted as the PDU following a CMGL/CMGR header.
	base := strings.TrimPrefix(command, "AT")
	if i := strings.IndexAny(base, "=?"); i >= 0 {
		base = base[:i]
	}
	if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+CME ERROR") && !strings.HasPrefix(line, "+CMS ERROR") && !strings.HasPrefix(line, base+":") {
		return
	}
	if ch != nil {
		select {
		case ch <- line:
		case <-s.done:
		default:
			s.Close()
		}
	}
}
func (s *Session) read() {
	defer s.Close()
	buf := make([]byte, 512)
	line := []byte{}
	for {
		n, e := s.port.Read(buf)
		if e != nil {
			return
		}
		for _, b := range buf[:n] {
			if b == '\r' || b == '\n' {
				s.emit(string(line))
				line = line[:0]
				continue
			}
			if b == '>' && strings.TrimSpace(string(line)) == "" {
				s.emit(">")
				line = line[:0]
				continue
			}
			line = append(line, b)
			if len(line) > 65536 {
				return
			}
		}
	}
}

func (s *Session) Do(ctx context.Context, command string) ([]string, error) {
	r, _, e := s.run(ctx, command, "")
	return r, e
}

// Submit reports started before writing the body: a partial write is ambiguous too.
func (s *Session) Submit(ctx context.Context, command, pdu string) ([]string, bool, error) {
	return s.run(ctx, command, pdu)
}
func (s *Session) run(ctx context.Context, command, pdu string) ([]string, bool, error) {
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, false, ctx.Err()
	case <-s.done:
		return nil, false, ErrClosed
	}
	defer func() { <-s.gate }()
	select {
	case <-s.done:
		return nil, false, ErrClosed
	default:
	}
	ch := make(chan string, 4096)
	s.mu.Lock()
	s.active = ch
	s.command = command
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.active = nil; s.command = ""; s.mu.Unlock() }()
	if _, e := io.WriteString(s.port, command+"\r"); e != nil {
		s.Close()
		return nil, false, e
	}
	lines := []string{}
	started := false
	for {
		select {
		case <-ctx.Done():
			s.Close()
			return lines, started, ctx.Err()
		case <-s.done:
			return lines, started, ErrClosed
		case l := <-ch:
			if l == command {
				continue
			}
			if l == ">" {
				if pdu == "" || started {
					s.Close()
					return lines, started, errors.New("unexpected AT prompt")
				}
				started = true
				if _, e := io.WriteString(s.port, pdu+"\x1a"); e != nil {
					s.Close()
					return lines, started, e
				}
				continue
			}
			if l == "OK" {
				if pdu != "" && !started {
					s.Close()
					return lines, false, errors.New("missing SMS prompt")
				}
				return lines, started, nil
			}
			if l == "ERROR" || strings.HasPrefix(l, "+CME ERROR") || strings.HasPrefix(l, "+CMS ERROR") {
				return lines, started, ErrProtocol
			}
			lines = append(lines, l)
			if len(lines) > 4096 {
				s.Close()
				return nil, started, errors.New("AT response exceeds bound")
			}
		}
	}
}
