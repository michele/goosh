package goosh

import (
	"encoding/json"
	"time"
)

type PushService interface {
	Process(Request) (Response, error)
}

type Request struct {
	PushID      string       `json:"push_id"`
	Multiplexed *Multiplexed `json:"multiplexed,omitempty"`
	Batched     *Batched     `json:"batched,omitempty"`
	APNSAuth    *APNSAuth    `json:"apns,omitempty"`
	FCMAuth     *FCMAuth     `json:"fcm,omitempty"`
	CustomID    string       `json:"custom_id"`
	iterator    int
	batchedKeys []string
	initialized bool
	total       int
	multiLen    int
}

// Multiplexed provide a single payload for multiple devices
type Multiplexed struct {
	Devices []string        `json:"devices"`
	Payload json.RawMessage `json:"payload"`
}

// Batched provides a payload for each device
type Batched map[string]json.RawMessage

func (r Request) Platform() string {
	if r.FCMAuth != nil && r.APNSAuth == nil {
		return "fcm"
	}
	if r.FCMAuth == nil && r.APNSAuth != nil {
		return "apns"
	}
	return ""
}

func (r Request) IsAPNS() bool {
	return r.Platform() == "apns"
}

func (r Request) IsFCM() bool {
	return r.Platform() == "fcm"
}

func (r *Request) Reset() {
	r.iterator = -1
}

func (r *Request) initialize() {
	if !r.initialized {
		r.Reset()
		r.initialized = true
		r.batchedKeys = []string{}
		if r.Batched != nil {
			for k, _ := range *r.Batched {
				r.batchedKeys = append(r.batchedKeys, k)
			}
		}
		if r.Multiplexed != nil {
			r.total += len(r.Multiplexed.Devices)
			r.multiLen = len(r.Multiplexed.Devices)
		}
		r.total += len(r.batchedKeys)
	}
}

func (r *Request) Next() bool {
	r.initialize()
	r.iterator++
	if r.iterator >= r.total {
		return false
	}
	return true
}

func (r *Request) Value() (msg Message) {
	if r.iterator >= r.total {
		return
	}
	if r.iterator < r.multiLen {
		msg.Payload = r.Multiplexed.Payload
		msg.Token = r.Multiplexed.Devices[r.iterator]
		return
	}
	offi := r.iterator - r.multiLen
	msg.Token = r.batchedKeys[offi]
	msg.Payload = (*r.Batched)[r.batchedKeys[offi]]
	return
}

func (r Request) Count() int64 {
	r.initialize()
	return int64(r.total)
}

type Message struct {
	Token   string
	Payload json.RawMessage
}

type Response struct {
	Failed   bool             `json:"failed"`
	Error    *Error           `json:"error,omitempty"`
	Devices  []DeviceResponse `json:"devices,omitempty"`
	Success  int64            `json:"success"`
	Failure  int64            `json:"failure"`
	PushID   string           `json:"push_id"`
	CustomID string           `json:"custom_id"`
}

type Error struct {
	Description string     `json:"description"`
	Code        int64      `json:"code"`
	ShouldRetry bool       `json:"should_retry"`
	RetryAt     *time.Time `json:"retry_at,omitempty"`
}

type DeviceResponse struct {
	Identifier  string `json:"identifier"`
	Delivered   bool   `json:"delivered"`
	Error       *Error `json:"error,omitempty"`
	ShouldRetry bool   `json:"should_retry,omitempty"`
	Canonical   string `json:"canonical,omitempty"`
}

type FCMAuth struct {
	AuthKey string `json:"auth_key"`
}

type APNSAuth struct {
	Certificate         string `json:"certificate"`
	CertificatePassword string `json:"certificate_password"`
	Sandbox             bool   `json:"sandbox"`
}
