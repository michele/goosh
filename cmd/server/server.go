package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/michele/factotum"
	"github.com/michele/goosh"
	"github.com/michele/goosh/services/apns2"
	"github.com/michele/goosh/services/fcm"
	"github.com/pkg/errors"
)

func main() {
	wait := sync.WaitGroup{}
	wait.Add(3)
	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt)
	wg := factotum.NewWorkerGroup(100)
	apns := apns2.NewPushService(wg.WorkQueue)
	fcm := fcm.NewPushService(wg.WorkQueue)
	cb := factotum.NewWorkerGroup(1)
	wg.Start()
	cb.Start()
	// Create our logger
	logger := log.New(os.Stdout, "", 0)

	s := NewServer(func(s *Server) { s.logger = logger }, func(s *Server) { s.apns = apns }, func(s *Server) { s.fcm = fcm }, func(s *Server) { s.cb = cb })

	h := &http.Server{Addr: ":8080", Handler: s}

	go func() {
		logger.Printf("Listening on http://0.0.0.0%s\n", ":8080")

		if err := h.ListenAndServe(); err != nil {
			logger.Printf("HTTP server stopped: %+v", err)
		}
		wait.Done()
	}()

	<-sigint
	logger.Println("\nShutting down the server...")
	s.goingAway = true
	go func() {
		wg.Stop()
		wait.Done()
	}()

	go func() {
		cb.Stop()
		wait.Done()
	}()

	ctx, _ := context.WithTimeout(context.Background(), 600*time.Second)

	go func() {
		h.Shutdown(ctx)
	}()
	wait.Wait()
	logger.Println("Bye bye...")
}

type Server struct {
	logger    *log.Logger
	mux       *http.ServeMux
	apns      goosh.PushService
	fcm       goosh.PushService
	cb        *factotum.WorkerGroup
	goingAway bool
}

func NewServer(options ...func(*Server)) *Server {
	s := &Server{
		logger: log.New(os.Stdout, "", 0),
		mux:    http.NewServeMux(),
	}

	for _, f := range options {
		f(s)
	}

	s.mux.HandleFunc("/healtz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); fmt.Fprintf(w, "OK") })
	s.mux.Handle("/push", s.pushHandler(s.cb, s.apns, s.fcm))

	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) pushHandler(cb *factotum.WorkerGroup, apns goosh.PushService, fcm goosh.PushService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.goingAway {
			w.WriteHeader(503)
			return
		}
		var req goosh.Request
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			err = errors.Wrap(err, "Couldn't read body")
			log.Printf("%+v", err)
			http.Error(w, "", 500)
			return
		}
		err = json.Unmarshal(body, &req)
		if err != nil {
			err = errors.Wrap(err, "Couldn't unmarshal body into request")
			log.Printf("%+v", err)
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
		Timeout: 10 * time.Second,
	}
	for sent == false && try < 10 {
		try++
		body, _ := json.Marshal(c.response)
		creq, _ := http.NewRequest("POST", c.url, ioutil.NopCloser(bytes.NewBuffer(body)))
		cres, err := cli.Do(creq)
		if err != nil {
			err = errors.Wrap(err, "couldn't trigger callback")
			log.Printf("Couldn't call callback: %+v", err)
		} else if cres.StatusCode >= 500 {
			err = errors.New("got error calling callback")
			log.Printf("Error calling callback: %+v", cres)
		} else if cres.StatusCode >= 400 {
			err = errors.New("something's not right with callback")
			log.Printf("Got a 4XX from callback: %+v", cres)
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