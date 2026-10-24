// Package notification builds the approved minimal Bark SMS presentation.
package notification

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"esim/internal/model"
	"esim/internal/sms"
)

// Reserve room for Bark's routing fields and APNs' envelope below 4096 bytes.
const MaxPayloadBytes = 3000

// DefaultIconURL is the Apple Messages artwork approved in the notification preview.
const DefaultIconURL = "https://is1-ssl.mzstatic.com/image/thumb/Purple221/v4/fc/8d/32/fc8d32a0-86a7-83d8-7045-57b4b3e71a0b/messagesCalistoga-0-0-1x_U007epad-0-1-0-85-220.png/512x512bb.jpg"

// Keep timestamps consistent on boards whose operating-system timezone is UTC.
var displayZone = time.FixedZone("Asia/Shanghai", 8*60*60)

func displayTime(m model.Message) string {
	t, err := time.Parse(time.RFC3339, m.SCTime)
	if err != nil {
		t = m.CreatedAt
	}
	return t.In(displayZone).Format("2006-01-02 15:04:05")
}

// Build creates durable segments containing the original SMS, a unified icon,
// an English title, and a sender/timestamp subtitle. No device metadata is shown.
// The completion flag stays in the interface for callers using schema v3.
func Build(message model.Message, spec model.ForwardSpec, _ bool) ([]model.Notification, error) {
	for budget := 2000; budget >= 32; budget = budget * 3 / 4 {
		parts := sms.SplitUTF8(message.Body, max(64, budget))
		out := make([]model.Notification, 0, len(parts))
		fits := true
		for i, text := range parts {
			if len(parts) > 1 {
				// SplitUTF8's generated numbering belongs in the title, not the SMS.
				text = strings.TrimPrefix(text, fmt.Sprintf("[%d/%d] ", i+1, len(parts)))
			}
			n := format(message, spec, text, i+1, len(parts))
			encoded, err := json.Marshal(n)
			if err != nil {
				return nil, err
			}
			if len(encoded) > MaxPayloadBytes {
				fits = false
				break
			}
			out = append(out, n)
		}
		if fits {
			return out, nil
		}
	}
	return nil, errors.New("notification metadata exceeds payload budget")
}

func format(m model.Message, spec model.ForwardSpec, text string, part, total int) model.Notification {
	sender := m.Number
	if sender == "" {
		sender = "Unknown sender"
	}
	n := model.Notification{
		Title: "New Message", Subtitle: sender + " · " + displayTime(m), Body: text,
		Icon: DefaultIconURL,
		// The identifier is invisible; retries and completed parts replace the
		// corresponding notification without exposing device names or SIM labels.
		ID: fmt.Sprintf("%s-message-%d-part-%d", spec.Group, m.ID, part),
	}
	if m.Status == "decode_error" {
		n.Title = "Message Unavailable"
		n.Body = "This message could not be decoded."
	} else if m.Status == "incomplete" {
		n.Title = "Incomplete Message"
	}
	if total > 1 {
		n.Title += fmt.Sprintf(" · %d/%d", part, total)
	}
	return n
}
