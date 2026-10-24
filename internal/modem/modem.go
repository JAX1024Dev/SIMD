package modem

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"esim/internal/at"
	"esim/internal/model"
	"esim/internal/sms"
)

type Driver interface {
	Initialize(context.Context) (model.Device, error)
	Status(context.Context) (model.Device, error)
	SIMIdentity(context.Context) (string, error)
	Scan(context.Context) ([]model.StoredSMS, error)
	Read(context.Context, string, int) (string, error)
	Delete(context.Context, string, int) error
	Send(context.Context, string, string) (bool, error)
	Notifications() <-chan struct{}
	Done() <-chan struct{}
	Close() error
}

type ML307R struct {
	session   *at.Session
	identity  model.Device
	stores    []string
	preferred string
}

func NewML307R(s *at.Session) *ML307R            { return &ML307R{session: s} }
func (m *ML307R) Close() error                   { return m.session.Close() }
func (m *ML307R) Done() <-chan struct{}          { return m.session.Done() }
func (m *ML307R) Notifications() <-chan struct{} { return m.session.Notifications() }
func (m *ML307R) do(ctx context.Context, cmd string) ([]string, error) {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return m.session.Do(c, cmd)
}
func value(lines []string, prefix string) string {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(l, prefix))
		}
	}
	return ""
}

func (m *ML307R) SIMIdentity(ctx context.Context) (string, error) {
	r, e := m.do(ctx, "AT+MCCID")
	if e != nil {
		return "", errors.New("SIM identity unavailable")
	}
	id := strings.Trim(value(r, "+MCCID:"), `" `)
	if len(id) < 10 || len(id) > 24 {
		return "", errors.New("invalid SIM identity")
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return "", errors.New("invalid SIM identity")
		}
	}
	return id, nil
}
func (m *ML307R) Initialize(ctx context.Context) (model.Device, error) {
	for _, c := range []string{"AT", "ATE0", "AT+CMEE=1"} {
		if _, e := m.do(ctx, c); e != nil {
			return m.identity, errors.New("AT initialization failed")
		}
	}
	for _, q := range []struct {
		cmd  string
		dest *string
	}{{"AT+CGMM", &m.identity.Model}, {"AT+CGMR", &m.identity.Firmware}, {"AT+CGSN", &m.identity.IMEI}} {
		r, e := m.do(ctx, q.cmd)
		if e != nil {
			return m.identity, errors.New("modem identification failed")
		}
		*q.dest = strings.Join(r, " ")
	}
	if !strings.Contains(strings.ToUpper(m.identity.Model), "ML307R") || strings.Contains(strings.ToUpper(m.identity.Model), "-DL") {
		m.identity.State = "unsupported"
		return m.identity, errors.New("unsupported modem model")
	}
	r, e := m.do(ctx, "AT+CPIN?")
	if e != nil {
		m.identity.State = "sim_missing"
		return m.identity, errors.New("SIM unavailable")
	}
	m.identity.SIMState = value(r, "+CPIN:")
	if m.identity.SIMState != "READY" {
		m.identity.State = "pin_required"
		return m.identity, errors.New("SIM requires manual PIN or PUK handling")
	}
	if _, e = m.do(ctx, "AT+CMGF=0"); e != nil {
		m.identity.State = "unsupported"
		return m.identity, errors.New("PDU SMS unsupported")
	}
	r, e = m.do(ctx, "AT+CPMS=?")
	if e != nil {
		return m.identity, errors.New("SMS storage unavailable")
	}
	groups := regexp.MustCompile(`\(([^)]*)\)`).FindAllStringSubmatch(strings.Join(r, ""), -1)
	if len(groups) < 3 {
		return m.identity, errors.New("invalid SMS storage capabilities")
	}
	for _, s := range []string{"SM", "ME"} {
		if strings.Contains(groups[0][1], `"`+s+`"`) {
			m.stores = append(m.stores, s)
		}
	}
	for _, s := range []string{"ME", "SM"} {
		if strings.Contains(groups[0][1], `"`+s+`"`) && strings.Contains(groups[1][1], `"`+s+`"`) && strings.Contains(groups[2][1], `"`+s+`"`) {
			m.preferred = s
			break
		}
	}
	if m.preferred == "" || len(m.stores) == 0 {
		return m.identity, errors.New("no usable SMS storage")
	}
	if _, e = m.do(ctx, fmt.Sprintf(`AT+CPMS="%s","%s","%s"`, m.preferred, m.preferred, m.preferred)); e != nil {
		return m.identity, errors.New("SMS storage configuration failed")
	}
	if _, e = m.do(ctx, "AT+CNMI=2,1,0,0,0"); e != nil {
		return m.identity, errors.New("stored SMS notifications unsupported")
	}
	m.identity.Capabilities = model.Capabilities{SMS: true, PDU: true, LongReceive: "unverified"}
	return m.Status(ctx)
}
func (m *ML307R) Status(ctx context.Context) (model.Device, error) {
	d := m.identity
	d.CheckedAt = time.Now().UTC()
	d.ICCID = ""
	d.State = "offline"
	r, e := m.do(ctx, "AT+CPIN?")
	if e != nil {
		d.State = "sim_missing"
		return d, errors.New("SIM unavailable")
	}
	d.SIMState = value(r, "+CPIN:")
	if d.SIMState != "READY" {
		d.State = "pin_required"
		return d, errors.New("SIM not ready")
	}
	r, e = m.do(ctx, "AT+MCCID")
	if e != nil {
		return d, errors.New("SIM identity unavailable")
	}
	d.ICCID = strings.Trim(value(r, "+MCCID:"), `" `)
	if len(d.ICCID) < 10 || len(d.ICCID) > 24 {
		return d, errors.New("invalid SIM identity")
	}
	for _, c := range d.ICCID {
		if c < '0' || c > '9' {
			return d, errors.New("invalid SIM identity")
		}
	}
	r, e = m.do(ctx, "AT+CEREG?")
	if e != nil {
		return d, errors.New("network registration query failed")
	}
	fields := strings.Split(value(r, "+CEREG:"), ",")
	if len(fields) < 2 {
		return d, errors.New("invalid network registration response")
	}
	d.Registration, e = strconv.Atoi(strings.TrimSpace(fields[1]))
	if e != nil {
		return d, e
	}
	if r, e = m.do(ctx, "AT+CSQ"); e == nil {
		v := strings.Split(value(r, "+CSQ:"), ",")
		if len(v) > 0 {
			d.Signal, _ = strconv.Atoi(strings.TrimSpace(v[0]))
		}
	}
	if r, e = m.do(ctx, "AT+COPS?"); e == nil {
		d.Operator = value(r, "+COPS:")
	}
	if m.preferred != "" {
		if _, e = m.do(ctx, fmt.Sprintf(`AT+CPMS="%s"`, m.preferred)); e != nil {
			return d, errors.New("SMS storage query failed")
		}
		if r, e = m.do(ctx, "AT+CPMS?"); e == nil {
			f := strings.Split(value(r, "+CPMS:"), ",")
			if len(f) >= 3 {
				d.Storage = strings.Trim(f[0], `" `)
				d.Used, _ = strconv.Atoi(strings.TrimSpace(f[1]))
				d.Total, _ = strconv.Atoi(strings.TrimSpace(f[2]))
			}
		} else {
			return d, errors.New("SMS storage query failed")
		}
	}
	d.State = "unregistered"
	if d.Registration == 1 || d.Registration == 5 || d.Registration == 6 || d.Registration == 7 {
		d.State = "ready"
	}
	return d, nil
}
func (m *ML307R) selectStore(ctx context.Context, storage string) error {
	valid := false
	for _, s := range m.stores {
		valid = valid || storage == s
	}
	if !valid {
		return errors.New("invalid SMS storage")
	}
	_, e := m.do(ctx, fmt.Sprintf(`AT+CPMS="%s"`, storage))
	return e
}
func (m *ML307R) Scan(ctx context.Context) ([]model.StoredSMS, error) {
	out := []model.StoredSMS{}
	for _, s := range m.stores {
		if e := m.selectStore(ctx, s); e != nil {
			return nil, e
		}
		r, e := m.do(ctx, "AT+CMGL=4")
		if e != nil {
			return nil, e
		}
		for i, l := range r {
			if !strings.HasPrefix(l, "+CMGL:") {
				continue
			}
			f := strings.Split(value([]string{l}, "+CMGL:"), ",")
			idx, e := strconv.Atoi(strings.TrimSpace(f[0]))
			if e != nil || idx < 0 || i+1 >= len(r) || len(f) < 2 {
				return nil, errors.New("invalid SMS list response")
			}
			state, err := strconv.Atoi(strings.TrimSpace(f[1]))
			if err != nil {
				return nil, errors.New("invalid SMS storage state")
			}
			if state != 0 && state != 1 {
				continue
			} // Leave stored outbound submissions untouched.
			out = append(out, model.StoredSMS{Storage: s, Index: idx, PDU: strings.TrimSpace(r[i+1])})
		}
	}
	return out, nil
}
func (m *ML307R) Read(ctx context.Context, storage string, index int) (string, error) {
	if index < 0 {
		return "", errors.New("invalid SMS index")
	}
	if e := m.selectStore(ctx, storage); e != nil {
		return "", e
	}
	r, e := m.do(ctx, fmt.Sprintf("AT+CMGR=%d", index))
	if e != nil {
		return "", e
	}
	for i, l := range r {
		if strings.HasPrefix(l, "+CMGR:") && i+1 < len(r) {
			return r[i+1], nil
		}
	}
	return "", errors.New("invalid SMS read response")
}
func (m *ML307R) Delete(ctx context.Context, storage string, index int) error {
	if index < 0 {
		return errors.New("invalid SMS index")
	}
	if e := m.selectStore(ctx, storage); e != nil {
		return e
	}
	_, e := m.do(ctx, fmt.Sprintf("AT+CMGD=%d,0", index))
	return e
}
func (m *ML307R) Send(ctx context.Context, number, body string) (bool, error) {
	pdu, n, e := sms.EncodeSubmit(number, body)
	if e != nil {
		return false, e
	}
	c, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	r, started, e := m.session.Submit(c, fmt.Sprintf("AT+CMGS=%d", n), pdu)
	if e != nil {
		if errors.Is(e, at.ErrProtocol) {
			return false, e
		}
		return started, e
	}
	if value(r, "+CMGS:") == "" {
		return true, errors.New("missing SMS submission confirmation")
	}
	return false, nil
}
