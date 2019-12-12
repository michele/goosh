package fcm

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/michele/factotum"
	"github.com/michele/goosh"
	"github.com/pkg/errors"
)

const fcmChunkSize = 1000
const fcmURI = "https://fcm.googleapis.com/fcm/send"
const initialBackoff = 5
const maxBackoff = 300

var (
	waitUntil   time.Time
	currentWait int64
	backoffLock sync.Mutex
)

type PushService struct {
	client          *client
	queue           chan factotum.WorkRequest
	Instrument      bool
	InstrumentPush  func(time.Duration)
	InstrumentError func(int)
}

type client struct {
	http *http.Client
}

type batch struct {
	devices []string
	payload string
	authKey string
}

type response struct {
	MulticastID  int64    `json:"multicast_id"`
	Success      int64    `json:"success"`
	Failure      int64    `json:"failure"`
	CanonicalIDs int64    `json:"canonical_ids"`
	Results      []result `json:"results"`
}

type result struct {
	MessageID string `json:"message_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

type workRequest struct {
	msg  goosh.Message
	res  chan<- goosh.DeviceResponse
	cli  *client
	akey string
	ps   *PushService
}

func NewPushService(q chan factotum.WorkRequest) (ps *PushService) {
	ps = &PushService{}
	ps.client = newClient()
	ps.queue = q
	return ps
}

func (r result) OK() bool {
	return r.Error == ""
}

func newClient() *client {
	newfcm := client{}

	tr := &http.Transport{
		MaxIdleConnsPerHost: 1024,
		TLSHandshakeTimeout: 0 * time.Second,
	}
	cli := &http.Client{
		Transport: tr,
	}

	newfcm.http = cli

	return &newfcm
}

func ShouldWait() bool {
	backoffLock.Lock()
	shoulda := waitUntil.After(time.Now())
	backoffLock.Unlock()
	return shoulda
}

func RetryAfter() int {
	if ShouldWait() {
		backoffLock.Lock()
		defer backoffLock.Unlock()
		return int(time.Now().Sub(waitUntil))
	}
	return 0
}

func (ps *PushService) Process(r goosh.Request) (resp goosh.Response, err error) {
	if r.Count() <= 0 {
		return
	}

	resp.CustomID = r.CustomID
	resp.Service = "fcm"
	results := make(chan goosh.DeviceResponse, 10)
	left := r.Count()
	go func() {
		for r.Next() {
			wr := workRequest{
				msg:  r.Value(),
				cli:  ps.client,
				res:  results,
				akey: r.FCMAuth.AuthKey,
				ps:   ps,
			}
			ps.queue <- wr
		}
	}()

	resps := []goosh.DeviceResponse{}
	var success int64
	var failed int64
	for ; left > 0; left-- {
		select {
		case dr, ok := <-results:
			if !ok {
				left = 0
			}
			resps = append(resps, dr)
			if dr.Delivered {
				success++
			} else {
				failed++
			}
		}
	}
	resp = goosh.Response{
		Devices:  resps,
		PushID:   r.PushID,
		Success:  success,
		Failure:  failed,
		CustomID: r.CustomID,
		Service:  "fcm",
	}
	return
}

func (ps *PushService) instrumentError(code int) {
	if ps.Instrument && ps.InstrumentError != nil {
		ps.InstrumentError(code)
	}
}

func (ps *PushService) instrumentPush(took time.Duration) {
	if ps.Instrument && ps.InstrumentPush != nil {
		ps.InstrumentPush(took)
	}
}

func (cli *client) push(authKey string, msg goosh.Message, ps *PushService) (goosh.DeviceResponse, error) {
	dr := goosh.DeviceResponse{
		Identifier: msg.Token,
	}
	payloadB, err := composePayload([]string{msg.Token}, msg.Payload)
	if err != nil {
		err = errors.Wrap(err, "composePayload returned an error")
		dr.Error = &goosh.Error{
			Code:        422,
			Description: "(pre-validation) invalid payload",
		}
		return dr, err
	}

	req, err := http.NewRequest("POST", fcmURI, ioutil.NopCloser(bytes.NewBuffer(payloadB)))
	if err != nil {
		err = errors.Wrap(err, "couldn't build FCM request")
		dr.Error = &goosh.Error{
			Code:        500,
			Description: "couldn't build request",
		}
		return dr, err
	}
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "key="+authKey)

	start := time.Now()
	resp, err := cli.http.Do(req)
	if err != nil {
		ps.instrumentError(599)
		err = errors.Wrap(err, "couldn't make POST request to FCM")
		wait := time.Now().Add(300 * time.Second)
		dr.Error = &goosh.Error{
			Code:        500,
			Description: "couldn't connect to FCM",
			ShouldRetry: true,
			RetryAt:     &wait,
		}
		dr.ShouldRetry = true
		return dr, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			ps.instrumentError(422)
			err = errors.Wrap(err, "couldn't read FCM response")
			dr.Error = &goosh.Error{
				Code:        422,
				Description: "couldn't read FCM response",
			}
			return dr, err
		}

		var fcmRes response
		err = json.Unmarshal(body, &fcmRes)
		if err != nil {
			ps.instrumentError(422)
			err = errors.Wrap(err, "couldn't unmarshal FCM response")
			dr.Error = &goosh.Error{
				Code:        422,
				Description: "couldn't parse FCM response",
			}
			return dr, err
		}

		r := fcmRes.Results[0]
		var e *goosh.Error
		if !r.OK() {
			e = &goosh.Error{}
			e.Description = r.Error
		}
		dr.Delivered = r.OK()
		dr.Error = e
		ps.instrumentPush(time.Now().Sub(start))
		return dr, nil
	} else if resp.StatusCode == 401 {
		ps.instrumentError(resp.StatusCode)
		dr.Error = &goosh.Error{
			Code:        401,
			Description: "wrong api key",
		}
		return dr, errors.New("wrong API key")
	} else if resp.StatusCode == 400 {
		ps.instrumentError(resp.StatusCode)
		dr.Error = &goosh.Error{
			Code:        400,
			Description: "invalid payload, check JSON",
		}
		return dr, errors.New("invalid payload, check JSON")
	} else if resp.StatusCode >= 500 {
		ps.instrumentError(resp.StatusCode)
		backoffLock.Lock()
		waitUntil = time.Now()
		retryAfter := resp.Header.Get("Retry-After")
		if retryAfter != "" {
			retryInt, err := strconv.Atoi(retryAfter)
			if err == nil {
				waitUntil.Add(time.Duration(retryInt) * time.Second)
			}
			if currentWait == 0 {
				currentWait = initialBackoff
			} else {
				currentWait = minInt(currentWait*2, maxBackoff)
			}
			waitUntil = waitUntil.Add(time.Duration(currentWait) * time.Second)
		}
		untilCopy := waitUntil
		backoffLock.Unlock()
		dr.Error = &goosh.Error{
			Code:        int64(resp.StatusCode),
			Description: "FCM error",
			ShouldRetry: true,
			RetryAt:     &untilCopy,
		}
		return dr, errors.New("FCM error")
	}

	ps.instrumentError(resp.StatusCode)
	dr.Error = &goosh.Error{
		Code:        int64(resp.StatusCode),
		Description: "Unknown response",
	}
	return dr, errors.New("Unknown response")
}

func (wr workRequest) Work() bool {
	dr, err := wr.cli.push(wr.akey, wr.msg, wr.ps)
	wr.res <- dr
	if err != nil {
		log.Printf("Got an error sending push: %+v", err)
		return false
	}
	return true
}

func composePayload(devices []string, payload json.RawMessage) ([]byte, error) {
	var parsed map[string]interface{}
	err := json.Unmarshal(payload, &parsed)
	if err != nil {
		err = errors.Wrap(err, "couldn't unmarshal user payload")
		return nil, err
	}
	parsed["registration_ids"] = devices

	payloadB, err := json.Marshal(parsed)
	if err != nil {
		err = errors.Wrap(err, "couldn't marshal payload for FCM")
		return nil, err
	}
	return payloadB, nil
}

func assignErrorToDevices(err *goosh.Error, devices []string, shouldRetry bool) []goosh.DeviceResponse {
	resp := []goosh.DeviceResponse{}
	for _, d := range devices {
		resp = append(resp, goosh.DeviceResponse{
			Error:       err,
			Identifier:  d,
			ShouldRetry: shouldRetry,
			Delivered:   false,
		})
	}
	return resp
}

func minInt(x, y int64) int64 {
	if x < y {
		return x
	}
	return y
}
