package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/michele/factotum"
	"github.com/michele/goosh/router"
	"github.com/michele/goosh/services/apns2"
	"github.com/michele/goosh/services/fcm"
)

func main() {
	logger := log.New(os.Stdout, "", 0)
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

	s := router.NewServer(func(s *router.Server) { s.Logger = logger }, func(s *router.Server) { s.APNS = apns }, func(s *router.Server) { s.FCM = fcm }, func(s *router.Server) { s.CB = cb })

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
	s.GoingAway = true
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
