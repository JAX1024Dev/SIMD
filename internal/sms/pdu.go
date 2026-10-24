// Package sms implements SMS-DELIVER decoding and single-part SMS-SUBMIT encoding.
package sms

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"esim/internal/model"
)

var gsm = []rune("@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞ\x1bÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà")
var ext = map[byte]rune{0x0a: '\f', 0x14: '^', 0x28: '{', 0x29: '}', 0x2f: '\\', 0x3c: '[', 0x3d: '~', 0x3e: ']', 0x40: '|', 0x65: '€'}

func GSMEncode(s string) ([]byte, bool) {
	out := []byte{}
	for _, r := range s {
		found := false
		for i, v := range gsm {
			if r == v && i != 27 {
				out = append(out, byte(i))
				found = true
				break
			}
		}
		if found {
			continue
		}
		for k, v := range ext {
			if r == v {
				out = append(out, 27, k)
				found = true
				break
			}
		}
		if !found {
			return nil, false
		}
	}
	return out, true
}
func gsmDecode(septets []byte) (string, error) {
	var b strings.Builder
	for i := 0; i < len(septets); i++ {
		n := septets[i]
		if n == 27 {
			i++
			if i == len(septets) {
				return "", errors.New("truncated GSM escape")
			}
			r, ok := ext[septets[i]]
			if !ok {
				return "", errors.New("unsupported GSM extension")
			}
			b.WriteRune(r)
		} else if int(n) < len(gsm) {
			b.WriteRune(gsm[n])
		} else {
			return "", errors.New("invalid septet")
		}
	}
	return b.String(), nil
}
func pack(s []byte) []byte {
	b := make([]byte, (len(s)*7+7)/8)
	for i, v := range s {
		for bit := 0; bit < 7; bit++ {
			if v&(1<<bit) != 0 {
				p := i*7 + bit
				b[p/8] |= 1 << uint(p%8)
			}
		}
	}
	return b
}
func unpack(b []byte, count int) ([]byte, error) {
	if count < 0 || count*7 > len(b)*8 {
		return nil, errors.New("truncated septets")
	}
	out := make([]byte, count)
	for i := range out {
		for bit := 0; bit < 7; bit++ {
			p := i*7 + bit
			if b[p/8]&(1<<uint(p%8)) != 0 {
				out[i] |= 1 << bit
			}
		}
	}
	return out, nil
}

// EncodeSubmit includes a zero-length SMSC address. length excludes that octet.
func EncodeSubmit(number, body string) (pdu string, length int, err error) {
	if !utf8.ValidString(body) || body == "" {
		return "", 0, errors.New("invalid or empty body")
	}
	n := strings.TrimPrefix(number, "+")
	if len(n) < 1 || len(n) > 20 {
		return "", 0, errors.New("invalid recipient")
	}
	for _, r := range n {
		if r < '0' || r > '9' {
			return "", 0, errors.New("recipient must contain digits with optional leading plus")
		}
	}
	toa := byte(0x81)
	if strings.HasPrefix(number, "+") {
		toa = 0x91
	}
	da := []byte{}
	for i := 0; i < len(n); i += 2 {
		lo := n[i] - '0'
		hi := byte(15)
		if i+1 < len(n) {
			hi = n[i+1] - '0'
		}
		da = append(da, lo|hi<<4)
	}
	dcs := byte(0)
	udl := 0
	var ud []byte
	if s, ok := GSMEncode(body); ok {
		if len(s) > 160 {
			return "", 0, errors.New("message_too_long")
		}
		udl = len(s)
		ud = pack(s)
	} else {
		dcs = 8
		for _, r := range body {
			if r > 0xffff || r >= 0xd800 && r <= 0xdfff {
				return "", 0, errors.New("unsupported_character")
			}
			ud = append(ud, byte(r>>8), byte(r))
		}
		if len(ud) > 140 {
			return "", 0, errors.New("message_too_long")
		}
		udl = len(ud)
	}
	b := []byte{0, 1, 0, byte(len(n)), toa}
	b = append(b, da...)
	b = append(b, 0, dcs, byte(udl))
	b = append(b, ud...)
	return strings.ToUpper(hex.EncodeToString(b)), len(b) - 1, nil
}

func Decode(raw string) (m model.DecodedSMS) {
	m.ConcatRef = -1
	fail := func(s string) model.DecodedSMS { m.Body = ""; m.Error = s; return m }
	if len(raw) > 2048 {
		return fail("PDU exceeds bound")
	}
	b, e := hex.DecodeString(strings.TrimSpace(raw))
	if e != nil || len(b) < 2 {
		return fail("invalid PDU hex")
	}
	p := 1 + int(b[0])
	if p >= len(b) {
		return fail("invalid SMSC length")
	}
	fo := b[p]
	p++
	if fo&3 != 0 {
		return fail("unsupported TPDU type")
	}
	if p+2 > len(b) {
		return fail("truncated address")
	}
	n, toa := int(b[p]), b[p+1]
	p += 2
	addrBytes := (n + 1) / 2
	if p+addrBytes+10 > len(b) {
		return fail("truncated SMS-DELIVER")
	}
	if toa&0x70 == 0x50 {
		s, err := unpack(b[p:p+addrBytes], n*4/7)
		if err != nil {
			return fail(err.Error())
		}
		m.Sender, e = gsmDecode(s)
		if e != nil {
			return fail(e.Error())
		}
	} else {
		var a strings.Builder
		if toa&0x70 == 0x10 {
			a.WriteByte('+')
		}
		for i := 0; i < n; i++ {
			v := b[p+i/2]
			if i%2 == 0 {
				v &= 15
			} else {
				v >>= 4
			}
			if v > 9 {
				return fail("unsupported numeric address")
			}
			a.WriteByte('0' + v)
		}
		m.Sender = a.String()
	}
	p += addrBytes
	p++ // protocol identifier
	dcs := b[p]
	p++
	m.SCTime, e = decodeTime(b[p : p+7])
	if e != nil {
		return fail(e.Error())
	}
	p += 7
	udl := int(b[p])
	p++
	ud := b[p:]
	header := 0
	alphabet := int((dcs >> 2) & 3)
	if dcs&0xc0 == 0 {
		if dcs&0x20 != 0 {
			return fail("compressed SMS unsupported")
		}
	} else if dcs&0xf0 == 0xf0 {
		alphabet = int((dcs >> 2) & 1)
	} else if dcs&0xe0 == 0xc0 {
		alphabet = 0
	} else if dcs&0xf0 == 0xe0 {
		alphabet = 2
	} else {
		return fail("unsupported DCS")
	}
	if fo&0x40 != 0 {
		if len(ud) == 0 {
			return fail("missing user data header")
		}
		header = int(ud[0]) + 1
		if header > len(ud) {
			return fail("truncated user data header")
		}
		for i := 1; i < header; {
			if i+2 > header {
				return fail("invalid UDH")
			}
			id, l := ud[i], int(ud[i+1])
			i += 2
			if i+l > header {
				return fail("invalid UDH element")
			}
			switch {
			case id == 0 && l == 3:
				m.ConcatRef = int(ud[i])
				m.ConcatTotal = int(ud[i+1])
				m.ConcatPart = int(ud[i+2])
			case id == 8 && l == 4:
				m.ConcatRef = int(ud[i])<<8 | int(ud[i+1])
				m.ConcatTotal = int(ud[i+2])
				m.ConcatPart = int(ud[i+3])
			case id == 0x24 || id == 0x25:
				return fail("national language shift unsupported")
			}
			i += l
		}
		if m.ConcatRef >= 0 && (m.ConcatTotal < 1 || m.ConcatPart < 1 || m.ConcatPart > m.ConcatTotal) {
			return fail("invalid concatenation metadata")
		}
	}
	switch alphabet {
	case 0:
		m.Encoding = "gsm7"
		if udl > 160 || len(ud) > 140 {
			return fail("invalid user data length")
		}
		ss, err := unpack(ud, udl)
		if err != nil {
			return fail(err.Error())
		}
		skip := (header*8 + 6) / 7
		if skip > len(ss) {
			return fail("UDH exceeds user data")
		}
		m.Body, e = gsmDecode(ss[skip:])
		if e != nil {
			return fail(e.Error())
		}
	case 2:
		m.Encoding = "ucs2"
		if udl > len(ud) || udl > 140 || header > udl || (udl-header)%2 != 0 {
			return fail("invalid UCS2 length")
		}
		u := []uint16{}
		for i := header; i < udl; i += 2 {
			u = append(u, uint16(ud[i])<<8|uint16(ud[i+1]))
		}
		for _, v := range u {
			if v >= 0xd800 && v <= 0xdfff {
				return fail("unsupported UCS2 surrogate")
			}
		}
		m.Body = string(utf16.Decode(u))
	case 1:
		m.Encoding = "binary"
		return fail("binary SMS retained without text decoding")
	default:
		return fail("unsupported alphabet")
	}
	return m
}

func decodeTime(b []byte) (string, error) {
	n := make([]int, 7)
	for i, v := range b {
		if i == 6 {
			v &^= 8
		}
		if v&15 > 9 || v>>4 > 9 {
			return "", errors.New("invalid SMS timestamp")
		}
		n[i] = int(v&15)*10 + int(v>>4)
	}
	y := 2000 + n[0]
	if n[0] >= 70 {
		y = 1900 + n[0]
	}
	offset := n[6] * 15 * 60
	if b[6]&8 != 0 {
		offset = -offset
	}
	if n[6] > 56 {
		return "", errors.New("invalid SMS timezone")
	}
	t := time.Date(y, time.Month(n[1]), n[2], n[3], n[4], n[5], 0, time.FixedZone("", offset))
	if int(t.Month()) != n[1] || t.Day() != n[2] || t.Hour() != n[3] || t.Minute() != n[4] || t.Second() != n[5] {
		return "", errors.New("invalid SMS timestamp")
	}
	return t.Format(time.RFC3339), nil
}

// SplitUTF8 bounds every notification body without splitting a code point.
func SplitUTF8(s string, max int) []string {
	if max < 64 {
		panic("notification body bound too small")
	}
	encoded, _ := json.Marshal(s)
	if len(s) <= max && len(encoded)-2 <= max {
		return []string{s}
	}
	parts := []string{}
	for len(s) > 0 {
		n, cost := 0, 0
		for n < len(s) {
			_, size := utf8.DecodeRuneInString(s[n:])
			unit, _ := json.Marshal(s[n : n+size])
			if cost+len(unit)-2 > max-24 {
				break
			}
			cost += len(unit) - 2
			n += size
		}
		parts = append(parts, s[:n])
		s = s[n:]
	}
	for i := range parts {
		parts[i] = "[" + strconv.Itoa(i+1) + "/" + strconv.Itoa(len(parts)) + "] " + parts[i]
	}
	return parts
}
