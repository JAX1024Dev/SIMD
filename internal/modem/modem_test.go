package modem

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"esim/internal/at"
	"esim/internal/testutil"
)

func hardware(t *testing.T, model string) *ML307R {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	go func() {
		r := bufio.NewReader(b)
		selected := "ME"
		for {
			line, e := r.ReadString('\r')
			if e != nil {
				return
			}
			cmd := strings.TrimSpace(line)
			response := ""
			switch {
			case cmd == "AT+CGMM":
				response = model
			case cmd == "AT+CGMR":
				response = "test-firmware"
			case cmd == "AT+CGSN":
				response = "123456789012345"
			case cmd == "AT+CPIN?":
				response = "+CPIN: READY"
			case cmd == "AT+MCCID":
				response = "+MCCID: 89860000000000000001"
			case cmd == "AT+CEREG?":
				response = "+CEREG: 0,1"
			case cmd == "AT+CSQ":
				response = "+CSQ: 20,99"
			case cmd == "AT+COPS?":
				response = `+COPS: 0,2,"46000",7`
			case cmd == "AT+CPMS=?":
				response = `+CPMS: ("SM","ME"),("SM","ME"),("SM","ME")`
			case cmd == "AT+CPMS?":
				response = fmt.Sprintf(`+CPMS: "%s",1,50,"ME",1,50,"ME",1,50`, selected)
			case strings.HasPrefix(cmd, "AT+CPMS="):
				if strings.Contains(cmd, `"SM"`) {
					selected = "SM"
				} else {
					selected = "ME"
				}
				response = "+CPMS: 1,50,1,50,1,50"
			case cmd == "AT+CMGL=4":
				response = "+CMGL: 1,0,,24\r\n" + testutil.Deliver
			case strings.HasPrefix(cmd, "AT+CMGR="):
				response = "+CMGR: 0,,24\r\n" + testutil.Deliver
			case strings.HasPrefix(cmd, "AT+CMGS="):
				io.WriteString(b, "\r\n> ")
				p, e := r.ReadString(26)
				if e != nil {
					return
				}
				if !strings.HasSuffix(p, "\x1a") {
					return
				}
				response = "+CMGS: 42"
			}
			if _, e = io.WriteString(b, "\r\n"+response+"\r\nOK\r\n"); e != nil {
				return
			}
		}
	}()
	return NewML307R(at.New(a))
}
func TestML307RInitializationScanAndSend(t *testing.T) {
	m := hardware(t, "ML307R")
	defer m.Close()
	ctx, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	d, e := m.Initialize(ctx)
	if e != nil || d.State != "ready" || d.ICCID == "" || !d.Capabilities.PDU {
		t.Fatalf("%#v %v", d, e)
	}
	rows, e := m.Scan(ctx)
	if e != nil || len(rows) != 2 || rows[0].Storage == rows[1].Storage {
		t.Fatalf("%v %v", rows, e)
	}
	p, e := m.Read(ctx, "ME", 1)
	if e != nil || p != testutil.Deliver {
		t.Fatal(e)
	}
	if e = m.Delete(ctx, "ME", 1); e != nil {
		t.Fatal(e)
	}
	if u, e := m.Send(ctx, "10086", "你好"); e != nil || u {
		t.Fatalf("%v %v", u, e)
	}
}
func TestRejectDLSKU(t *testing.T) {
	m := hardware(t, "ML307R-DL")
	defer m.Close()
	d, e := m.Initialize(context.Background())
	if e == nil || d.State != "unsupported" {
		t.Fatalf("%#v %v", d, e)
	}
}
