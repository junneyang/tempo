package pool

import (
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/uber-go/atomic"
)

const (
	queueLengthReportDuration = 15 * time.Second
)

var (
	metricQueryQueueLength = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "tempodb",
		Name:      "work_queue_length",
		Help:      "Current length of the work queue.",
	})

	metricQueryQueueMax = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "tempodb",
		Name:      "work_queue_max",
		Help:      "Maximum number of items in the work queue.",
	})
)

type JobFunc func(payload interface{}) ([]byte, error)

type job struct {
	payload interface{}
	fn      JobFunc

	wg        *sync.WaitGroup
	resultsCh chan []byte
	stopCh    chan struct{}
	err       *atomic.Error
}

type Pool struct {
	cfg  *Config
	size *atomic.Int32

	workQueue  chan *job
	shutdownCh chan struct{}
}

func NewPool(cfg *Config) *Pool {
	if cfg == nil {
		cfg = defaultConfig()
	}

	q := make(chan *job, cfg.QueueDepth)
	p := &Pool{
		cfg:        cfg,
		workQueue:  q,
		size:       atomic.NewInt32(0),
		shutdownCh: make(chan struct{}),
	}

	for i := 0; i < cfg.MaxWorkers; i++ {
		go p.worker(q)
	}

	p.reportQueueLength()

	metricQueryQueueMax.Set(float64(cfg.QueueDepth))

	return p
}

func (p *Pool) RunJobs(payloads []interface{}, fn JobFunc) ([]byte, error) {
	totalJobs := len(payloads)

	// sanity check before we even attempt to start adding jobs
	if int(p.size.Load())+totalJobs > p.cfg.QueueDepth {
		return nil, fmt.Errorf("queue doesn't have room for %d jobs", len(payloads))
	}

	resultsCh := make(chan []byte, 1) // way for jobs to send back results
	err := atomic.NewError(nil)       // way for jobs to send back an error
	stopCh := make(chan struct{})     // way to signal to the jobs to quit
	wg := &sync.WaitGroup{}           // way to wait for all jobs to complete

	wg.Add(totalJobs)
	// add each job one at a time.  even though we checked length above these might still fail
	for _, payload := range payloads {
		j := &job{
			fn:        fn,
			payload:   payload,
			wg:        wg,
			resultsCh: resultsCh,
			stopCh:    stopCh,
			err:       err,
		}

		select {
		case p.workQueue <- j:
			p.size.Inc()
		default:
			close(stopCh)
			return nil, fmt.Errorf("failed to add a job to work queue")
		}
	}

	jobsDoneCh := make(chan struct{}, 1)
	go func() {
		wg.Wait()
		jobsDoneCh <- struct{}{}
	}()

	var msg []byte
	closed := false
	for {
		select {
		case msg = <-resultsCh:
			wg.Done()
			if !closed {
				close(stopCh)
				closed = true
			}
		case <-jobsDoneCh:
			if msg != nil {
				return msg, nil
			}
			return nil, err.Load()
		}
	}
}

func (p *Pool) Shutdown() {
	close(p.workQueue)
	close(p.shutdownCh)
}

func (p *Pool) worker(j <-chan *job) {
	for {
		select {
		case <-p.shutdownCh:
			return
		case j, ok := <-j:
			if !ok {
				return
			}
			runJob(j)
			p.size.Dec()
		}
	}
}

func (p *Pool) reportQueueLength() {
	ticker := time.NewTicker(queueLengthReportDuration)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				metricQueryQueueLength.Set(float64(p.size.Load()))
			case <-p.shutdownCh:
				return
			}
		}
	}()
}

func runJob(job *job) {
	select {
	case <-job.stopCh:
		job.wg.Done()
		return
	default:
		msg, err := job.fn(job.payload)

		if msg != nil {
			select {
			case job.resultsCh <- msg:
				// not signalling done here to dodge race condition between resultsCh and stopCh
				return
			default:
			}
		}
		if err != nil {
			job.err.Store(err)
		}
		job.wg.Done()
	}
}

// default is concurrency disabled
func defaultConfig() *Config {
	return &Config{
		MaxWorkers: 30,
		QueueDepth: 10000,
	}
}
