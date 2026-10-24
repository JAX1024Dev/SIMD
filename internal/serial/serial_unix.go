//go:build linux || darwin

package serial

import (
	"errors"
	"io"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Port uses non-blocking I/O and bounded polls so Close always unblocks Read.
type Port struct {
	mu     sync.RWMutex
	fd     int
	closed bool
}

var ErrBusy = errors.New("serial port is occupied")

func Open(path string, baud int, flow bool) (*Port, error) {
	switch baud {
	case 300, 1200, 2400, 4800, 9600, 19200, 38400, 57600, 115200, 230400:
	default:
		return nil, errors.New("unsupported serial baud rate")
	}
	fd, e := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if e != nil {
		if errors.Is(e, unix.EBUSY) {
			return nil, ErrBusy
		}
		return nil, e
	}
	bad := func(e error) (*Port, error) { unix.Close(fd); return nil, e }
	if e = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); e != nil {
		return bad(ErrBusy)
	}
	if e = unix.IoctlSetInt(fd, unix.TIOCEXCL, 0); e != nil {
		return bad(e)
	}
	if e = configure(fd, baud, flow); e != nil {
		return bad(e)
	}
	return &Port{fd: fd}, nil
}
func (p *Port) Read(b []byte) (int, error) {
	for {
		p.mu.RLock()
		if p.closed {
			p.mu.RUnlock()
			return 0, io.EOF
		}
		fds := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}}
		n, e := unix.Poll(fds, 200)
		if e == unix.EINTR {
			p.mu.RUnlock()
			continue
		}
		if e != nil {
			p.mu.RUnlock()
			return 0, e
		}
		if n == 0 {
			p.mu.RUnlock()
			continue
		}
		if fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			p.mu.RUnlock()
			return 0, io.EOF
		}
		n, e = unix.Read(p.fd, b)
		p.mu.RUnlock()
		if e == unix.EAGAIN || e == unix.EINTR {
			continue
		}
		if n == 0 && e == nil {
			return 0, io.EOF
		}
		return n, e
	}
}
func (p *Port) Write(b []byte) (int, error) {
	total := 0
	deadline := time.Now().Add(5 * time.Second)
	for len(b) > 0 {
		p.mu.RLock()
		if p.closed {
			p.mu.RUnlock()
			return total, io.ErrClosedPipe
		}
		fds := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLOUT}}
		_, e := unix.Poll(fds, 200)
		if e != nil && e != unix.EINTR {
			p.mu.RUnlock()
			return total, e
		}
		if time.Now().After(deadline) {
			p.mu.RUnlock()
			return total, errors.New("serial write timeout")
		}
		n, e := unix.Write(p.fd, b)
		p.mu.RUnlock()
		if e == unix.EAGAIN || e == unix.EINTR {
			continue
		}
		if e != nil {
			return total, e
		}
		total += n
		b = b[n:]
	}
	return total, nil
}
func (p *Port) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	unix.IoctlSetInt(p.fd, unix.TIOCNXCL, 0)
	return unix.Close(p.fd)
}
