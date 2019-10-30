package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"git.sr.ht/~mmf/queuer"
	"github.com/michele/goosh"
	"github.com/pkg/errors"
)

var (
	ErrQueueNotAvailable = errors.New("Queue isn't set on the client")
)

type Client struct {
	http     *http.Client
	host     string
	port     string
	protocol string
	queue    queuer.Queue
}

func NewClient(protocol, host, port string) *Client {
	if host == "" {
		host = "localhost"
	}

	if port == "" {
		port = "8080"
	}

	if protocol == "" {
		protocol = "http"
	}

	c := &Client{
		host:     host,
		port:     port,
		protocol: protocol,
	}

	tr := &http.Transport{
		MaxIdleConnsPerHost: 1024,
		TLSHandshakeTimeout: 0 * time.Second,
	}

	c.http = &http.Client{
		Transport: tr,
	}

	return c
}

func (c *Client) SetQueue(q queuer.Queue) {
	c.queue = q
}

func (c *Client) HTTP() *http.Client {
	return c.http
}

func (c *Client) DoAsync(gr goosh.Request, callback string) error {
	body, err := json.Marshal(gr)

	if err != nil {
		err = errors.Wrap(err, "Couldn't marshal body")
		return err
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("%s://%s:%s/push?callback=%s", c.protocol, c.host, c.port, callback), ioutil.NopCloser(bytes.NewBuffer(body)))

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	if err != nil {
		err = errors.Wrap(err, "Couldn't build request")
		return err
	}

	res, err := c.http.Do(req)

	if err != nil {
		err = errors.Wrap(err, "Couldn't call goosh")
		return err
	}

	if res.StatusCode >= 300 {
		err = errors.New(fmt.Sprintf("Something went wrong while calling goosh [%d]", res.StatusCode))
		return err
	}
	return nil
}

func (c *Client) Do(gr goosh.Request) (*goosh.Response, error) {
	body, err := json.Marshal(gr)

	if err != nil {
		err = errors.Wrap(err, "Couldn't marshal body")
		return nil, err
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("%s://%s:%s/push", c.protocol, c.host, c.port), ioutil.NopCloser(bytes.NewBuffer(body)))

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	if err != nil {
		err = errors.Wrap(err, "Couldn't build request")
		return nil, err
	}

	res, err := c.http.Do(req)

	if err != nil {
		err = errors.Wrap(err, "Couldn't call goosh")
		return nil, err
	}

	if res.StatusCode >= 300 {
		err = errors.New(fmt.Sprintf("Something went wrong while calling goosh [%d]", res.StatusCode))
		return nil, err
	}

	defer res.Body.Close()
	body, err = ioutil.ReadAll(res.Body)

	if err != nil {
		return nil, errors.Wrap(err, "Couldn't read response body")
	}

	var gresp goosh.Response
	err = json.Unmarshal(body, &gresp)

	if err != nil {
		return nil, errors.Wrap(err, "Couldn't parse JSON response")
	}
	return &gresp, nil
}

func (c *Client) Enqueue(gr *goosh.Request) error {
	if c.queue == nil {
		return ErrQueueNotAvailable
	}
	bts, err := json.Marshal(gr)

	if err != nil {
		return errors.Wrap(err, "Goosh#Enqueue: couldn't marshal request")
	}

	err = c.queue.Publish(bts)

	if err != nil {
		return errors.Wrap(err, "Goosh#Enqueue: couldn't push request into queue")
	}

	return nil
}

func (c *Client) Pop() (*goosh.Response, error) {
	if c.queue == nil {
		return nil, ErrQueueNotAvailable
	}

	obj := <-c.queue.Receive()

	var gr goosh.Response
	err := json.Unmarshal(obj.Body(), &gr)

	if err != nil {
		return nil, errors.Wrap(err, "Goosh#Enqueue: couldn't unmarshal request")
	}

	return &gr, nil
}
