package qflow

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"github.com/threecommaio/qflow/pkg/durable"
)

type Endpoint struct {
	Name           string
	Hosts          []string
	Writer         chan interface{}
	DurableChannel chan interface{}
	WorkerChannel  chan *durable.Request
	Timeout        time.Duration
}

type Handler struct {
	Endpoints []Endpoint
}

var (
	endpointLatencyHistogram = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "endpoint_latency_us",
		Help:    "Endpoint latency distributions in microseconds",
		Buckets: prometheus.ExponentialBuckets(0.5, 1.3, 50),
	}, []string{"endpoint"})

	endpointRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "endpoint_requests",
		Help: "Number of requests",
	}, []string{"endpoint"})

	endpointFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "endpoint_failures",
		Help: "Number of failed requests",
	}, []string{"endpoint"})

	requests = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "requests",
		Help: "Number of incoming requests",
	})

	failures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "failures",
		Help: "Number of incoming failed requests",
	})
)

// HTTPWorker handles making the remote HTTP calls with a bounded channel concurrency
func HTTPWorker(endpoint *Endpoint) {
	var count int
	var sizeEndpoints = len(endpoint.Hosts)
	var microInNS = time.Microsecond.Nanoseconds()

	defaultRoundTripper := http.DefaultTransport
	defaultTransport := defaultRoundTripper.(*http.Transport)
	defaultTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // ignore expired SSL certificates
	client := &http.Client{Timeout: endpoint.Timeout, Transport: defaultTransport}

	for {
		req := <-endpoint.WorkerChannel
		r := bytes.NewReader(req.Body)
		url := fmt.Sprintf("%s%s", endpoint.Hosts[count%sizeEndpoints], req.URL)
		proxyReq, err := http.NewRequest(req.Method, url, r)
		if err != nil {
			log.Debugf("error: %s", err)
			continue
		}

		start := time.Now()
		endpointRequests.WithLabelValues(endpoint.Name).Inc()
		proxyRes, err := client.Do(proxyReq)

		respLatencyNS := time.Since(start).Nanoseconds()
		elasped := float64(respLatencyNS / microInNS)
		endpointLatencyHistogram.WithLabelValues(endpoint.Name).Observe(elasped)

		if err != nil {
			endpointFailures.WithLabelValues(endpoint.Name).Inc()
			log.Debugf("error: %s", err)
			endpoint.Writer <- req
			continue
		}

		io.Copy(ioutil.Discard, proxyRes.Body)
		proxyRes.Body.Close()
	}
}

// ReadDiskChannel handles reading from the disk backed channel
func ReadDiskChannel(endpoint *Endpoint) {
	var count int
	for {
		item := <-endpoint.DurableChannel
		req := item.(durable.Request)
		count++

		if count%1000 == 0 {
			log.Debug("processed 1000 operations")
		}
		endpoint.WorkerChannel <- &req
	}
}

// HandleRequest handles processing every request sent
func (h *Handler) HandleRequest(w http.ResponseWriter, req *http.Request) {
	requests.Inc()
	body, err := ioutil.ReadAll(req.Body)
	defer req.Body.Close()

	if err != nil {
		failures.Inc()
		log.Debugf("error reading body: %v", err)
		http.Error(w, "can't read body", http.StatusBadRequest)
		return
	}

	r := &durable.Request{Method: req.Method, URL: req.URL.String(), Body: body}
	for _, endpoint := range h.Endpoints {
		endpoint.Writer <- r
	}

	// 200 - StatusNoContent
	w.WriteHeader(http.StatusNoContent)
}

// ListenAndServe will startup an http server and handle proxying requests
func ListenAndServe(config *Config, addr string, dataDir string) {
	var ep []Endpoint
	var timeout = config.HTTP.Timeout
	var maxMsgSize = config.Queue.MaxMessageSize
	var concurrency = config.HTTP.Concurrency

	if timeout.Seconds() == 0.0 {
		timeout = 10 * time.Second
	}

	if maxMsgSize == 0 {
		maxMsgSize = 1024 * 1024 * 10 // 10mb
	}

	if concurrency == 0 {
		concurrency = 25
	}

	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		log.Infof("creating data directory: %s", dataDir)
		err = os.MkdirAll(dataDir, 0755)
		if err != nil {
			log.Fatal(err)
		}
	}

	// register prometheus metrics
	prometheus.MustRegister(requests, failures, endpointLatencyHistogram, endpointRequests, endpointFailures)

	for _, endpoint := range config.Endpoints {
		for _, host := range endpoint.Hosts {
			if !isValidURL(host) {
				log.Fatalf("(%s) [%s] is not a valid endpoint url", endpoint.Name, host)
			}
		}

		log.Infof("registered (%s) with endpoints: [%s]", endpoint.Name, strings.Join(endpoint.Hosts, ","))
		log.Infof("config options: (http timeout: %s, maxMsgSize: %d, concurrency: %d)",
			timeout,
			maxMsgSize,
			concurrency)

		writer := make(chan interface{})
		worker := make(chan *durable.Request, concurrency)

		c := durable.Channel(writer, &durable.Config{
			Name:            endpoint.Name,
			DataPath:        dataDir,
			MaxBytesPerFile: 1024 * 1024 * 1024,
			MinMsgSize:      0,
			MaxMsgSize:      maxMsgSize,
			SyncEvery:       10000,
			SyncTimeout:     time.Second * 10,
		})

		e := &Endpoint{
			Name:           endpoint.Name,
			Hosts:          endpoint.Hosts,
			Writer:         writer,
			DurableChannel: c,
			WorkerChannel:  worker,
			Timeout:        timeout,
		}
		ep = append(ep, *e)

		for i := 0; i < concurrency; i++ {
			go HTTPWorker(e)
		}

		go ReadDiskChannel(e)

	}

	handler := &Handler{Endpoints: ep}
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/", handler.HandleRequest)

	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// isValidURL handles checking if a url is valid
func isValidURL(s string) bool {
	url, err := url.ParseRequestURI(s)

	if url.Scheme != "http" && url.Scheme != "https" {
		return false
	}

	if err != nil {
		return false
	} else {
		return true
	}
}
