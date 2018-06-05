package apns2

import (
	"bytes"
	"crypto"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io/ioutil"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/michele/factotum"
	"github.com/michele/goosh"
	"github.com/pkg/errors"
	"golang.org/x/net/http2"
)

var (
	ErrFailedToDecryptKey           = errors.New("failed to decrypt private key")
	ErrFailedToParsePKCS1PrivateKey = errors.New("failed to parse PKCS1 private key")
	ErrFailedToParseCertificate     = errors.New("failed to parse certificate PEM data")
	ErrNoPrivateKey                 = errors.New("no private key")
	ErrNoCertificate                = errors.New("no certificate")
)

type PushService struct {
	clients         map[string]client
	lock            sync.Mutex
	queue           chan factotum.WorkRequest
	Instrument      bool
	InstrumentPush  func(time.Duration)
	InstrumentError func(int)
}

type client struct {
	http         *http.Client
	cacheKey     string
	production   bool
	pemData      []byte
	certificates tls.Certificate
	topic        string
}
type push struct {
	pushID    string
	client    client
	responses []goosh.DeviceResponse
	results   chan<- goosh.DeviceResponse
}

type workRequest struct {
	msg goosh.Message
	res chan<- goosh.DeviceResponse
	cli *client
	ps  *PushService
}

type response struct {
	Reason string `json:"reason"`
}

func NewPushService(q chan factotum.WorkRequest) (ps *PushService) {
	ps = &PushService{}
	ps.clients = map[string]client{}
	ps.queue = q
	return ps
}

func cacheKey(r goosh.Request) (string, error) {
	key, err := base64.StdEncoding.DecodeString(r.APNSAuth.Certificate)
	if err != nil {
		err = errors.Wrap(err, "couldn't decode apns certificate")
		return "", err
	}
	key = append(key, []byte(r.APNSAuth.CertificatePassword)...)
	if r.APNSAuth.Sandbox {
		key = append(key, []byte("true")...)
	} else {
		key = append(key, []byte("false")...)
	}
	return GetMD5Hash(key), nil
}

func newClient(ck string, r goosh.Request) (cli client, err error) {
	pemData, err := base64.StdEncoding.DecodeString(r.APNSAuth.Certificate)
	if err != nil {
		err = errors.Wrap(err, "couldn't decode apns certificate")
		return
	}
	//if !sandbox {
	rxp := regexp.MustCompile(`(?mi)^\s*friendlyName: [^:]+ Push Services: (.*)$`)
	ss := rxp.FindSubmatch(pemData)
	if len(ss) > 0 {
		cli.topic = string(ss[1])
	}
	//}

	cli.pemData = pemData

	if r.APNSAuth.Sandbox {
		cli.production = false
	} else {
		cli.production = true
	}

	certs, err := FromPemBytes(pemData, r.APNSAuth.CertificatePassword)
	if err != nil {
		err = errors.Wrap(err, "couldn't parse PEM certificate")
		return
	}

	conf := &tls.Config{
		Certificates: []tls.Certificate{certs},
		NextProtos:   []string{"h2"},
	}
	hcli := &http.Client{
		Transport: &http2.Transport{
			TLSClientConfig: conf,
		},
	}

	cli.certificates = certs
	cli.http = hcli

	return
}

func (c *client) host() string {
	if c.production {
		return "api.push.apple.com"
	}
	return "api.development.push.apple.com"
}

func (c *client) urlForDevice(device string) string {
	return "https://" + c.host() + "/3/device/" + device
}

func (c *client) Push(m goosh.Message, ps *PushService) (goosh.DeviceResponse, error) {
	body, _ := json.Marshal(m.Payload)
	device := m.Token
	dres := goosh.DeviceResponse{}
	dres.Identifier = device
	uid := uuid.New().String()
	req, err := http.NewRequest("POST", c.urlForDevice(device), ioutil.NopCloser(bytes.NewBuffer([]byte(body))))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Apns-Id", uid)
	if c.topic != "" {
		req.Header.Add("Apns-Topic", c.topic)
	}

	if err != nil {
		err = errors.Wrap(err, "error building APNS request")
		dres.Error = &goosh.Error{Description: "error building APNS request"}
		return dres, err
	}
	//resp, err := client.Post(, "application/json", )
	not_sent := true
	retries := 5
	start := time.Now()
	var resp *http.Response
	for not_sent {
		resp, err = c.http.Do(req)
		if err != nil {
			ps.instrumentError(599)
			if retries <= 0 {
				errors.Wrap(err, "couldn't make request to APNS")
				wait := time.Now().Add(300 * time.Second)
				dres.Error = &goosh.Error{ShouldRetry: true, RetryAt: &wait, Code: 502, Description: "couldn't make request to APNS"}
				return dres, err
			}
			retries--
			req.Body = ioutil.NopCloser(bytes.NewBuffer([]byte(body)))
			time.Sleep(500 * time.Millisecond)
		} else {
			not_sent = false
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		ioutil.ReadAll(resp.Body)
		dres.Delivered = true
	} else {
		ps.instrumentError(resp.StatusCode)
		apnsError := goosh.Error{Code: int64(resp.StatusCode)}
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			err = errors.Wrap(err, "couldn't read APNS response")
			apnsError.Description = "couldn't read APNS response"
			dres.Error = &apnsError
			return dres, err
		}
		var parsedErr response
		err = json.Unmarshal(body, &parsedErr)
		if err != nil {
			err = errors.Wrap(err, "couldn't parse APNS response")
			apnsError.Description = "couldn't parse APNS response"
			dres.Error = &apnsError
			return dres, err
		}
		apnsError.Description = parsedErr.Reason
		if resp.StatusCode >= 500 {
			apnsError.ShouldRetry = true
			wait := time.Now().Add(300 * time.Second)
			apnsError.RetryAt = &wait
		}
		dres.Error = &apnsError
	}
	if resp.StatusCode == 200 {
		ps.instrumentPush(time.Now().Sub(start))
	}
	return dres, nil
}

func (wr workRequest) Work() bool {
	dr, err := wr.cli.Push(wr.msg, wr.ps)
	wr.res <- dr
	if err != nil {
		log.Printf("Got an error sending push: %+v", err)
		return false
	}
	return true
}

func (ps *PushService) getClient(r goosh.Request) (cli client, err error) {
	var ok bool
	var ck string
	ps.lock.Lock()
	ck, err = cacheKey(r)
	if err != nil {
		err = errors.Wrap(err, "Couldn't get cacheKey")
		return
	}
	cli, ok = ps.clients[ck]
	if !ok {
		cli, err = newClient(ck, r)
		if err != nil {
			err = errors.Wrap(err, "Couldn't setup new client")
			return
		}
		ps.clients[ck] = cli
	}
	ps.lock.Unlock()
	return
}

func (ps *PushService) Process(r goosh.Request) (resp goosh.Response, err error) {
	if r.Count() <= 0 {
		return
	}
	var cli client
	cli, err = ps.getClient(r)

	if err != nil {
		// TODO: Setup response with error
		err = errors.Wrap(err, "Couldn't get client")
		return
	}
	results := make(chan goosh.DeviceResponse, 10)
	left := r.Count()
	go func() {
		for r.Next() {
			wr := workRequest{
				msg: r.Value(),
				cli: &cli,
				res: results,
				ps:  ps,
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
		Devices: resps,
		PushID:  r.PushID,
		Success: success,
		Failure: failed,
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

func GetMD5Hash(text []byte) string {
	hasher := md5.New()
	hasher.Write(text)
	return hex.EncodeToString(hasher.Sum(nil))
}

// FromPemBytes loads a PEM certificate from an in memory byte array and
// returns a tls.Certificate. This function is similar to the crypto/tls
// X509KeyPair function, however it supports PEM files with the cert and
// key combined, as well as password protected keys which are both common with
// APNs certificates.
//
// Use "" as the password argument if the PEM certificate is not password
// protected.
func FromPemBytes(bytes []byte, password string) (tls.Certificate, error) {
	var cert tls.Certificate
	var block *pem.Block
	for {
		block, bytes = pem.Decode(bytes)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			cert.Certificate = append(cert.Certificate, block.Bytes)
		}
		if block.Type == "PRIVATE KEY" || strings.HasSuffix(block.Type, "PRIVATE KEY") {
			key, err := unencryptPrivateKey(block, password)
			if err != nil {
				return tls.Certificate{}, err
			}
			cert.PrivateKey = key
		}
	}
	if len(cert.Certificate) == 0 {
		return tls.Certificate{}, ErrNoCertificate
	}
	if cert.PrivateKey == nil {
		return tls.Certificate{}, ErrNoPrivateKey
	}
	if c, e := x509.ParseCertificate(cert.Certificate[0]); e == nil {
		cert.Leaf = c
	}
	return cert, nil
}

func unencryptPrivateKey(block *pem.Block, password string) (crypto.PrivateKey, error) {
	if x509.IsEncryptedPEMBlock(block) {
		bytes, err := x509.DecryptPEMBlock(block, []byte(password))
		if err != nil {
			return nil, ErrFailedToDecryptKey
		}
		return parsePrivateKey(bytes)
	}
	return parsePrivateKey(block.Bytes)
}

func parsePrivateKey(bytes []byte) (crypto.PrivateKey, error) {
	key, err := x509.ParsePKCS1PrivateKey(bytes)
	if err != nil {
		return nil, ErrFailedToParsePKCS1PrivateKey
	}
	return key, nil
}
