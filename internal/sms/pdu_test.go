package sms

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestKnownDeliverVectors(t *testing.T) {
	for _, tc := range []struct{ pdu, sender, body string }{{"00040B912143658709F10000620142214365230AE8329BFD4697D9EC37", "+12345678901", "hellohello"}, {"000405810180F6000862014221436523044F60597D", "10086", "你好"}} {
		m := Decode(tc.pdu)
		if m.Error != "" || m.Sender != tc.sender || m.Body != tc.body || m.SCTime != "2026-10-24T12:34:56+08:00" {
			t.Fatalf("decoded %#v", m)
		}
	}
}
func TestSinglePartEncoding(t *testing.T) {
	for _, tc := range []struct {
		body string
		ok   bool
	}{{strings.Repeat("a", 160), true}, {strings.Repeat("a", 161), false}, {strings.Repeat("^", 80), true}, {strings.Repeat("^", 81), false}, {strings.Repeat("你", 70), true}, {strings.Repeat("你", 71), false}, {"emoji 😀", false}, {"", false}, {"a\x1a b", true}} {
		p, n, e := EncodeSubmit("+8613800138000", tc.body)
		if (e == nil) != tc.ok {
			t.Errorf("body bytes %d: %v", len(tc.body), e)
		}
		if e == nil && len(p)/2-1 != n {
			t.Fatal("CMGS length includes SMSC")
		}
	}
	for _, n := range []string{"", "++123", "123\rAT", "１２３", strings.Repeat("1", 21)} {
		if _, _, e := EncodeSubmit(n, "ok"); e == nil {
			t.Fatalf("accepted invalid recipient %q", n)
		}
	}
	p, _, e := EncodeSubmit("10086", "你好")
	if e != nil || p != "00010005810180F60008044F60597D" {
		t.Fatalf("unexpected SMS-SUBMIT %s %v", p, e)
	}
}
func TestConcatAndMalformed(t *testing.T) {
	seven := Decode("004405810180F60000620142214365230C0500037A0201D06536FB0D")
	if seven.Error != "" || seven.Body != "hello" || seven.ConcatPart != 1 {
		t.Fatalf("GSM7 UDH fill bits %#v", seven)
	}
	m := Decode("004405810180F60008620142214365230A0500037A02014F60597D")
	if m.Error != "" || m.Body != "你好" || m.ConcatRef != 122 || m.ConcatTotal != 2 || m.ConcatPart != 1 {
		t.Fatalf("concat %#v", m)
	}
	for _, p := range []string{"", "GG", "FF00", "004405810180F600086201422143652306FF0000000000", "000405810180F6000862014221436523034F60", "000405810180F60004620142214365230101"} {
		m := Decode(p)
		if m.Error == "" {
			t.Errorf("malformed or binary PDU accepted: %s", p)
		}
	}
}

func TestNotificationEscapingBudget(t *testing.T) {
	s := strings.Repeat("\x01<>&\n\\\"", 300)
	parts := SplitUTF8(s, 2000)
	if len(parts) < 2 {
		t.Fatal("JSON escaping was not budgeted")
	}
	var restored strings.Builder
	for _, p := range parts {
		encoded, _ := json.Marshal(p)
		if len(encoded)-2 > 2000 {
			t.Fatal("notification too large after JSON escaping")
		}
		restored.WriteString(p[strings.Index(p, "] ")+2:])
	}
	if restored.String() != s {
		t.Fatal("lost escaped content")
	}
}
func TestGSMAlphabetAndSplit(t *testing.T) {
	if len(gsm) != 128 {
		t.Fatalf("alphabet length %d", len(gsm))
	}
	for i, r := range gsm {
		if i == 27 {
			continue
		}
		s, ok := GSMEncode(string(r))
		if !ok {
			t.Fatal(r)
		}
		u, e := unpack(pack(s), len(s))
		if e != nil {
			t.Fatal(e)
		}
		got, e := gsmDecode(u)
		if e != nil || got != string(r) {
			t.Fatalf("character %q", r)
		}
	}
	s := strings.Repeat("短信😀", 1000)
	parts := SplitUTF8(s, 2000)
	var restored strings.Builder
	for _, p := range parts {
		if !utf8.ValidString(p) || len(p) > 2000 {
			t.Fatal("invalid split")
		}
		restored.WriteString(p[strings.Index(p, "] ")+2:])
	}
	if restored.String() != s {
		t.Fatal("split lost content")
	}
}
func FuzzDecodeNeverPanics(f *testing.F) {
	f.Add("000405810180F6000862014221436523044F60597D")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) { Decode(s) })
}
