package at

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestURCInterleavingAndPromptWithoutNewline(t *testing.T) {
	a, b := net.Pipe()
	s := New(a)
	defer s.Close()
	defer b.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := bufio.NewReader(b)
		cmd, _ := r.ReadString('\r')
		if cmd != "AT+CMGS=10\r" {
			return
		}
		io.WriteString(b, "\r\n+CMTI: \"ME\",3\r\n> ")
		p, _ := r.ReadString(26)
		if p != "ABCD\x1a" {
			return
		}
		io.WriteString(b, "\r\n+CMTI: \"SM\",4\r\n+CMGS: 23\r\nOK\r\n")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, started, e := s.Submit(ctx, "AT+CMGS=10", "ABCD")
	if e != nil || !started || len(r) != 1 || r[0] != "+CMGS: 23" {
		t.Fatalf("%v %v %v", r, started, e)
	}
	select {
	case <-s.Notifications():
	default:
		t.Fatal("missing SMS indication")
	}
	<-done
}

func TestRegistrationURCDoesNotBecomePDU(t *testing.T) {
	a, b := net.Pipe()
	s := New(a)
	defer s.Close()
	defer b.Close()
	go func() {
		r := bufio.NewReader(b)
		r.ReadString('\r')
		io.WriteString(b, "\r\n+CMGR: 0,,2\r\n+CEREG: 1\r\nABCD\r\nOK\r\n")
		r.ReadString('\r')
		io.WriteString(b, "\r\n+CEREG: 0,1\r\nOK\r\n")
	}()
	r, e := s.Do(context.Background(), "AT+CMGR=1")
	if e != nil || len(r) != 2 || r[1] != "ABCD" {
		t.Fatalf("%v %v", r, e)
	}
	r, e = s.Do(context.Background(), "AT+CEREG?")
	if e != nil || len(r) != 1 || r[0] != "+CEREG: 0,1" {
		t.Fatalf("%v %v", r, e)
	}
}
func TestCommandSerialization(t *testing.T) {
	a, b := net.Pipe()
	s := New(a)
	defer s.Close()
	defer b.Close()
	go func() {
		r := bufio.NewReader(b)
		for {
			cmd, e := r.ReadString('\r')
			if e != nil {
				return
			}
			io.WriteString(b, "\r\n"+strings.TrimSpace(cmd)+"\r\nOK\r\n")
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, c := context.WithTimeout(context.Background(), time.Second)
			defer c()
			if _, e := s.Do(ctx, "AT"); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
}
func TestTimeoutClosesDesynchronizedSession(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	s := New(a)
	defer s.Close()
	go func() { r := bufio.NewReader(b); r.ReadString('\r'); io.WriteString(b, "\r\n> "); r.ReadString(26) }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, started, e := s.Submit(ctx, "AT+CMGS=1", "00")
	if !started || !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("%v %v", started, e)
	}
	if _, e = s.Do(context.Background(), "AT"); !errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
}
