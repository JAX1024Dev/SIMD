package notification

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"esim/internal/model"
)

func message(body string) model.Message {
	return model.Message{ID: 42, DeviceID: "sim-01", Number: "10086", Body: body, Status: "received", SCTime: "2026-10-25T00:15:00+08:00", CreatedAt: time.Date(2026, 10, 24, 16, 15, 0, 0, time.UTC)}
}
func spec() model.ForwardSpec {
	return model.ForwardSpec{DeviceName: "Main Modem", SIMLabel: "Personal SIM", Group: "simd-sim-01"}
}
func TestApprovedMinimalPresentation(t *testing.T) {
	text := "【测试】您的验证码为 123456，5 分钟内有效。"
	notes, err := Build(message(text), spec(), false)
	if err != nil || len(notes) != 1 {
		t.Fatal(err)
	}
	n := notes[0]
	if n.Title != "New Message" || n.Subtitle != "10086 · 2026-10-25 00:15:00" || n.Body != text || n.Icon != DefaultIconURL {
		t.Fatalf("incorrect presentation: title=%q subtitle=%q", n.Title, n.Subtitle)
	}
	encoded, _ := json.Marshal(n)
	var payload map[string]any
	json.Unmarshal(encoded, &payload)
	if len(payload) != 5 {
		t.Fatalf("unexpected extra display fields: %v", payload)
	}
	for _, key := range []string{"title", "subtitle", "body", "icon", "id"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("missing %s", key)
		}
	}
	if n.ID != "simd-sim-01-message-42-part-1" {
		t.Fatal("unstable identity")
	}
}
func TestDeviceMetadataNeverDisplayed(t *testing.T) {
	s := spec()
	s.DeviceName = "主模组"
	s.SIMLabel = "个人卡"
	for _, text := range []string{"验证码为 123456。", "余额为 1234.56 元。", "[*text*](https://example.com)\nNext line"} {
		out, err := Build(message(text), s, false)
		if err != nil {
			t.Fatal(err)
		}
		n := out[0]
		if n.Title != "New Message" || n.Body != text || n.Group != "" || n.Markdown != "" || n.Copy != "" || n.Action != "" || n.Sound != "" || n.Level != "" || n.Archive != "" {
			t.Fatal("unexpected decoration or altered message")
		}
		if strings.Contains(n.Title+n.Subtitle+n.Body, "主模组") || strings.Contains(n.Subtitle, "sim-01") {
			t.Fatal("device metadata displayed")
		}
	}
}
func TestStatesUseUnifiedIconAndStableCompletion(t *testing.T) {
	m := message("短信 [Missing part 2/2]")
	m.Status = "incomplete"
	partial, err := Build(m, spec(), false)
	if err != nil {
		t.Fatal(err)
	}
	if partial[0].Title != "Incomplete Message" {
		t.Fatal("missing incomplete state")
	}
	m.Status = "received"
	m.Body = "完整短信"
	complete, err := Build(m, spec(), true)
	if err != nil || complete[0].ID != partial[0].ID || complete[0].Title != "New Message" {
		t.Fatal("completion identity changed")
	}
	m.Status = "decode_error"
	m.Number = ""
	m.Body = ""
	unavailable, err := Build(m, spec(), false)
	if err != nil || unavailable[0].Title != "Message Unavailable" || unavailable[0].Body != "This message could not be decoded." || !strings.HasPrefix(unavailable[0].Subtitle, "Unknown sender · ") {
		t.Fatal("invalid failure presentation")
	}
	for _, n := range []model.Notification{partial[0], complete[0], unavailable[0]} {
		if n.Icon != DefaultIconURL {
			t.Fatal("icon varies by message kind")
		}
	}
}
func TestJSONBudgetAndLosslessSegments(t *testing.T) {
	for _, text := range []string{strings.Repeat("短信😀\n", 1000), strings.Repeat("<>&\\\"\x01", 400), strings.Repeat("[1/2] literal marker\n", 200)} {
		out, err := Build(message(text), spec(), false)
		if err != nil {
			t.Fatal(err)
		}
		var restored strings.Builder
		for i, n := range out {
			encoded, err := json.Marshal(n)
			if err != nil || len(encoded) > MaxPayloadBytes || !utf8.ValidString(n.Body) {
				t.Fatal("oversized or invalid notification")
			}
			if len(out) > 1 && n.Title != fmt.Sprintf("New Message · %d/%d", i+1, len(out)) {
				t.Fatal("incorrect segment numbering")
			}
			if n.Subtitle != "10086 · 2026-10-25 00:15:00" {
				t.Fatal("subtitle changed across segments")
			}
			restored.WriteString(n.Body)
		}
		if restored.String() != text {
			t.Fatal("SMS text modified by segmentation")
		}
	}
}
func TestTimestampFallbackAndTimezone(t *testing.T) {
	m := message("hello")
	m.SCTime = "invalid"
	notes, err := Build(m, spec(), false)
	if err != nil || notes[0].Subtitle != "10086 · 2026-10-25 00:15:00" {
		t.Fatal("received time fallback incorrect")
	}
	m.SCTime = "2026-10-25T00:15:00Z"
	notes, err = Build(m, spec(), false)
	if err != nil || notes[0].Subtitle != "10086 · 2026-10-25 08:15:00" {
		t.Fatal("display depends on host timezone")
	}
}
