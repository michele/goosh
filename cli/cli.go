package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/michele/goosh"
	"github.com/pkg/errors"
)

type Client struct {
	http     *http.Client
	host     string
	port     string
	protocol string
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
