package model

import "time"

type Capabilities struct {
	SMS         bool   `json:"sms"`
	PDU         bool   `json:"pdu"`
	LongReceive string `json:"long_receive"`
}

type Device struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	SIMLabel     string       `json:"sim_label"`
	Phone        string       `json:"phone,omitempty"`
	Enabled      bool         `json:"enabled"`
	State        string       `json:"state"`
	Model        string       `json:"model,omitempty"`
	Firmware     string       `json:"firmware,omitempty"`
	IMEI         string       `json:"imei,omitempty"`
	ICCID        string       `json:"iccid,omitempty"`
	SIMState     string       `json:"sim_state,omitempty"`
	Operator     string       `json:"operator,omitempty"`
	Registration int          `json:"registration"`
	Signal       int          `json:"signal"`
	Used         int          `json:"storage_used"`
	Total        int          `json:"storage_total"`
	Storage      string       `json:"storage,omitempty"`
	Capabilities Capabilities `json:"capabilities"`
	LastError    string       `json:"last_error,omitempty"`
	CheckedAt    time.Time    `json:"checked_at"`
}

type Message struct {
	ID         int64     `json:"id"`
	DeviceID   string    `json:"device_id"`
	ICCID      string    `json:"iccid"`
	Direction  string    `json:"direction"`
	Number     string    `json:"number"`
	Body       string    `json:"body"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	SCTime     string    `json:"smsc_time,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	Historical bool      `json:"historical"`
	Raw        []string  `json:"raw_pdus,omitempty"`
}

type ForwardTask struct {
	ID           int64        `json:"id"`
	MessageID    int64        `json:"message_id"`
	TargetID     string       `json:"target_id"`
	ParentID     *int64       `json:"parent_id,omitempty"`
	Part         int          `json:"part"`
	Parts        int          `json:"parts"`
	Title        string       `json:"-"`
	Body         string       `json:"-"`
	Group        string       `json:"-"`
	Notification Notification `json:"-"`
	Status       string       `json:"status"`
	Attempts     int          `json:"attempts"`
	LastError    string       `json:"last_error,omitempty"`
	CreatedAt    time.Time    `json:"created_at"`
	NextAt       time.Time    `json:"next_at"`
	ExpiresAt    time.Time    `json:"expires_at"`
}

type ForwardSpec struct{ TargetID, Title, Body, Group, DeviceName, SIMLabel string }

// Notification is the durable presentation snapshot sent to Bark. Values remain
// strings for compatibility with both JSON and key-path Bark API variants.
type Notification struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	Body     string `json:"body"`
	Markdown string `json:"markdown,omitempty"`
	Group    string `json:"group,omitempty"`
	Icon     string `json:"icon,omitempty"`
	Sound    string `json:"sound,omitempty"`
	Level    string `json:"level,omitempty"`
	Copy     string `json:"copy,omitempty"`
	Action   string `json:"action,omitempty"`
	Archive  string `json:"isArchive,omitempty"`
	ID       string `json:"id,omitempty"`
}
type StoredSMS struct {
	Storage string
	Index   int
	PDU     string
}

type DecodedSMS struct {
	Sender      string
	Body        string
	SCTime      string
	Encoding    string
	ConcatRef   int
	ConcatTotal int
	ConcatPart  int
	Error       string
}

type SendTask struct {
	MessageID int64
	DeviceID  string
	ICCID     string
	Number    string
	Body      string
	CreatedAt time.Time
}
