package worker

import (
	"sync"
)

type WorkRequest interface {
	Work() bool
}

type Worker struct {
	ID          int
	Work        chan WorkRequest
	WorkerQueue chan chan WorkRequest
	Quit        chan bool
	wait        *sync.WaitGroup
}

type WorkerGroup struct {
	WorkerQueue chan chan WorkRequest
	WorkQueue   chan WorkRequest
	workers     []*Worker
	wait        *sync.WaitGroup
	closed      bool
	quit        chan bool
}

func NewWorker(id int, wq chan chan WorkRequest, wait *sync.WaitGroup) *Worker {
	worker := &Worker{
		ID:          id,
		Work:        make(chan WorkRequest),
		WorkerQueue: wq,
		Quit:        make(chan bool),
		wait:        wait,
	}

	return worker
}

func (w *Worker) Start() {
	go func() {
		defer w.wait.Done()
		for {
			w.WorkerQueue <- w.Work

			select {
			case work := <-w.Work:
				work.Work()
			case <-w.Quit:
				return
			}
		}
	}()
}

func (w *Worker) Stop() {
	go func() {
		close(w.Quit)
	}()
}

func NewWorkerGroup(n int) (wg *WorkerGroup) {
	wg = &WorkerGroup{}
	wg.WorkerQueue = make(chan chan WorkRequest, n)
	wg.WorkQueue = make(chan WorkRequest, n*2)
	wg.workers = make([]*Worker, n)
	wg.quit = make(chan bool)
	wg.wait = &sync.WaitGroup{}
	wg.wait.Add(n)
	for i := 0; i < n; i++ {
		w := NewWorker(i+1, wg.WorkerQueue, wg.wait)
		wg.workers[i] = w
		w.Start()
	}
	return wg
}

func (wg *WorkerGroup) Start() {
	go func() {
		for {
			select {
			case work := <-wg.WorkQueue:
				go func() {
					worker := <-wg.WorkerQueue

					worker <- work
				}()
			case <-wg.quit:
				return
			}
		}
	}()
}

func (wg *WorkerGroup) Enqueue(w WorkRequest) bool {
	if wg.closed {
		return false
	}
	go func() {
		wg.WorkQueue <- w
	}()
	return true
}

func (wg *WorkerGroup) Stop() {
	if wg.closed {
		return
	}
	close(wg.quit)
	wg.closed = true
	for _, w := range wg.workers {
		w.Stop()
	}
	wg.wait.Wait()
	close(wg.WorkQueue)
	close(wg.WorkerQueue)
}
