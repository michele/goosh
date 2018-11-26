package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/michele/factotum"
	"github.com/michele/goosh"
	"github.com/pkg/errors"
)

const defaultCallbackTimeout = 30

var callbackTimeout = defaultCallbackTimeout

func init() {
	var err error
	if len(os.Getenv("GOOSH_CALLBACK_TIMEOUT")) > 0 {
		callbackTimeout, err = strconv.Atoi(os.Getenv("GOOSH_CALLBACK_TIMEOUT"))
		if err != nil {
			log.Printf("Couldn't parse ENV GOOSH_CALLBACK_TIMEOUT. Using default (%d) instead.", defaultCallbackTimeout)
			callbackTimeout = defaultCallbackTimeout
		}
	}
}

type Server struct {
	Logger    *log.Logger
	mux       *http.ServeMux
	APNS      goosh.PushService
	FCM       goosh.PushService
	CB        *factotum.WorkerGroup
	GoingAway bool
}

func NewServer(options ...func(*Server)) *Server {
	s := &Server{
		Logger: log.New(os.Stdout, "", 0),
		mux:    http.NewServeMux(),
	}

	for _, f := range options {
		f(s)
	}

	s.mux.HandleFunc("/healtz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); fmt.Fprintf(w, "OK") })
	s.mux.Handle("/push", s.pushHandler(s.CB, s.APNS, s.FCM))

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) pushHandler(cb *factotum.WorkerGroup, apns goosh.PushService, fcm goosh.PushService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.GoingAway {
			w.WriteHeader(503)
			return
		}
		var req goosh.Request
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			err = errors.Wrap(err, "Couldn't read body")
			log.Printf("%+v\nThis was the request: %+v", err, r)
			http.Error(w, "", 500)
			return
		}
		err = json.Unmarshal(body, &req)
		if err != nil {
			err = errors.Wrap(err, "Couldn't unmarshal body into request")
			log.Printf("%+v\nThis was the body: %s", err, string(body))
			http.Error(w, "", 400)
			return
		}
		var dr goosh.Response
		callbackURL := r.URL.Query().Get("callback")
		var procFunc func(goosh.Request) (goosh.Response, error)
		if req.IsFCM() {
			procFunc = fcm.Process
		} else if req.IsAPNS() {
			procFunc = apns.Process
		} else {
			http.Error(w, "", 422)
			return
		}
		if callbackURL != "" {
			go func() {
				dr, _ = procFunc(req)
				cb.Enqueue(callback{response: dr, url: callbackURL})
			}()
			w.WriteHeader(http.StatusAccepted)
		} else {
			dr, _ = procFunc(req)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(dr)
		}
	})
}

type callback struct {
	url      string
	response goosh.Response
}

func (c callback) Work() bool {
	sent := false
	try := 0
	wait := 5
	cli := http.Client{
		Timeout: time.Duration(callbackTimeout) * time.Second,
	}
	for sent == false && try < 10 {
		try++
		body, err := json.Marshal(c.response)
		if err != nil {
			err = errors.Wrap(err, "couldn't marshal response")
			log.Printf("Couldn't marshal response: %+v\nThis was the response: %+v", err, c.response)
			continue
		}
		creq, err := http.NewRequest("POST", c.url, ioutil.NopCloser(bytes.NewBuffer(body)))
		if err != nil {
			err = errors.Wrap(err, "couldn't build HTTP request")
			log.Printf("Couldn't build request: %+v\nURL: %s\nThis was the response: %+v", err, c.url, c.response)
			continue
		}
		cres, err := cli.Do(creq)
		if err != nil {
			err = errors.Wrap(err, "couldn't trigger callback")
			log.Printf("Couldn't call callback: %+v\nThis was the body: %s", err, string(body))
		} else if cres.StatusCode >= 500 {
			err = errors.New("got error calling callback")
			log.Printf("Error calling callback: %+v\nThis was the body: %s", cres, string(body))
		} else if cres.StatusCode >= 400 {
			err = errors.New("something's not right with callback")
			log.Printf("Got a 4XX from callback: %+v\nThis was the body: %s", cres, string(body))
			sent = true
		} else {
			sent = true
		}
		if sent {
			break
		}

		time.Sleep(time.Duration(wait) * time.Second)
		wait = wait * 2
	}
	return true
}

func (s *Server) withMetrics(l *log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		began := time.Now()
		next.ServeHTTP(w, r)
		l.Printf("%s %s took %s", r.Method, r.URL, time.Since(began))
	})
}
