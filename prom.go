// Package ginprom is a library to instrument a gin server and expose a
// /metrics endpoint for Prometheus to scrape, keeping a low cardinality by
// preserving the path parameters name in the prometheus label
package ginprom

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var defaultPath = "/metrics"
var defaultNs = "gin"
var defaultSys = "gonic"
var defaultHandlerNameFunc = (*gin.Context).HandlerName
var defaultRequestPathFunc = (*gin.Context).FullPath

var defaultReqCntMetricName = "requests_total"
var defaultReqDurMetricName = "request_duration"
var defaultReqSzMetricName = "request_size_bytes"
var defaultResSzMetricName = "response_size_bytes"
var defaultVersionMetricName = "version"
var defaultUpTimeMetricName = "up_time"

// ErrInvalidToken is returned when the provided token is invalid or missing.
var ErrInvalidToken = errors.New("invalid or missing token")

// ErrCustomGauge is returned when the custom gauge can't be found.
var ErrCustomGauge = errors.New("error finding custom gauge")

// ErrCustomCounter is returned when the custom counter can't be found.
var ErrCustomCounter = errors.New("error finding custom counter")

type pmapb struct {
	sync.RWMutex
	values map[string]bool
}

type pmapGauge struct {
	sync.RWMutex
	values map[string]prometheus.GaugeVec
}

type pmapCounter struct {
	sync.RWMutex
	values map[string]prometheus.CounterVec
}

// Prometheus contains the metrics gathered by the instance and its path.
type Prometheus struct {
	reqCnt       *prometheus.CounterVec
	version      prometheus.GaugeFunc
	upTime       *prometheus.CounterVec
	reqDur       *prometheus.HistogramVec
	reqSz, resSz prometheus.Summary

	customGauges   pmapGauge
	customCounters pmapCounter

	MetricsPath     string
	Namespace       string
	Subsystem       string
	Token           string
	Ignored         pmapb
	Engine          *gin.Engine
	BucketsSize     []float64
	Registry        *prometheus.Registry
	HandlerNameFunc func(c *gin.Context) string
	RequestPathFunc func(c *gin.Context) string

	RequestCounterMetricName  string
	RequestDurationMetricName string
	RequestSizeMetricName     string
	ResponseSizeMetricName    string
	UpTimeMetricName          string
	VersionMetricName         string
}

// IncrementGaugeValue increments a custom gauge.
func (p *Prometheus) IncrementGaugeValue(name string, labelValues []string) error {
	p.customGauges.RLock()
	defer p.customGauges.RUnlock()

	if g, ok := p.customGauges.values[name]; ok {
		g.WithLabelValues(labelValues...).Inc()
	} else {
		return ErrCustomGauge
	}
	return nil
}

// SetGaugeValue sets gauge to value.
func (p *Prometheus) SetGaugeValue(name string, labelValues []string, value float64) error {
	p.customGauges.RLock()
	defer p.customGauges.RUnlock()

	if g, ok := p.customGauges.values[name]; ok {
		g.WithLabelValues(labelValues...).Set(value)
	} else {
		return ErrCustomGauge
	}
	return nil
}

// AddGaugeValue adds value to custom gauge.
func (p *Prometheus) AddGaugeValue(name string, labelValues []string, value float64) error {
	p.customGauges.RLock()
	defer p.customGauges.RUnlock()

	if g, ok := p.customGauges.values[name]; ok {
		g.WithLabelValues(labelValues...).Add(value)
	} else {
		return ErrCustomGauge
	}
	return nil
}

// DecrementGaugeValue decrements a custom gauge.
func (p *Prometheus) DecrementGaugeValue(name string, labelValues []string) error {
	p.customGauges.RLock()
	defer p.customGauges.RUnlock()

	if g, ok := p.customGauges.values[name]; ok {
		g.WithLabelValues(labelValues...).Dec()
	} else {
		return ErrCustomGauge
	}
	return nil
}

// SubGaugeValue adds gauge to value.
func (p *Prometheus) SubGaugeValue(name string, labelValues []string, value float64) error {
	p.customGauges.RLock()
	defer p.customGauges.RUnlock()

	if g, ok := p.customGauges.values[name]; ok {
		g.WithLabelValues(labelValues...).Sub(value)
	} else {
		return ErrCustomGauge
	}
	return nil
}

// AddCustomGauge adds a custom gauge and registers it.
func (p *Prometheus) AddCustomGauge(name, help string, labels []string) {
	p.customGauges.Lock()
	defer p.customGauges.Unlock()

	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: p.Namespace,
		Subsystem: p.Subsystem,
		Name:      name,
		Help:      help,
	},
		labels)
	p.customGauges.values[name] = *g
	p.mustRegister(g)
}

// IncrementCounterValue increments a custom counter.
func (p *Prometheus) IncrementCounterValue(name string, labelValues []string) error {
	p.customCounters.RLock()
	defer p.customCounters.RUnlock()

	if g, ok := p.customCounters.values[name]; ok {
		g.WithLabelValues(labelValues...).Inc()
	} else {
		return ErrCustomCounter
	}
	return nil
}

// AddCounterValue adds value to custom counter.
func (p *Prometheus) AddCounterValue(name string, labelValues []string, value float64) error {
	p.customCounters.RLock()
	defer p.customCounters.RUnlock()

	if g, ok := p.customCounters.values[name]; ok {
		g.WithLabelValues(labelValues...).Add(value)
	} else {
		return ErrCustomCounter
	}
	return nil
}

// AddCustomCounter adds a custom counter and registers it.
func (p *Prometheus) AddCustomCounter(name, help string, labels []string) {
	p.customCounters.Lock()
	defer p.customCounters.Unlock()
	g := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: p.Namespace,
		Subsystem: p.Subsystem,
		Name:      name,
		Help:      help,
	}, labels)
	p.customCounters.values[name] = *g
	p.mustRegister(g)
}

func (p *Prometheus) mustRegister(c ...prometheus.Collector) {
	registerer, _ := p.getRegistererAndGatherer()
	registerer.MustRegister(c...)
}

// New will initialize a new Prometheus instance with the given options.
// If no options are passed, sane defaults are used.
// If a router is passed using the Engine() option, this instance will
// automatically bind to it.
func New(options ...PrometheusOption) *Prometheus {
	p := &Prometheus{
		MetricsPath:               defaultPath,
		Namespace:                 defaultNs,
		Subsystem:                 defaultSys,
		HandlerNameFunc:           defaultHandlerNameFunc,
		RequestPathFunc:           defaultRequestPathFunc,
		RequestCounterMetricName:  defaultReqCntMetricName,
		RequestDurationMetricName: defaultReqDurMetricName,
		RequestSizeMetricName:     defaultReqSzMetricName,
		ResponseSizeMetricName:    defaultResSzMetricName,
		UpTimeMetricName:          defaultUpTimeMetricName,
	}
	p.customGauges.values = make(map[string]prometheus.GaugeVec)
	p.customCounters.values = make(map[string]prometheus.CounterVec)

	p.Ignored.values = make(map[string]bool)
	for _, option := range options {
		option(p)
	}

	p.register()
	if p.Engine != nil {
		registerer, gatherer := p.getRegistererAndGatherer()
		p.Engine.GET(p.MetricsPath, prometheusHandler(p.Token, registerer, gatherer))
	}
	go p.recordUptime()

	return p
}

// SetVersionInfo registers constant version info
// versionInfo: ie:
//
//	map[string]string{
//		 "version": "V1.0.1",
//		 "language": "Go 1.19.7",
//		 "author": "@Depado",
//	}
func (p *Prometheus) SetVersionInfo(versionInfo map[string]string) {
	p.VersionMetricName = defaultVersionMetricName
	p.version = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace:   p.Namespace,
			Subsystem:   p.Subsystem,
			Name:        p.VersionMetricName,
			Help:        "Version.",
			ConstLabels: versionInfo,
		}, func() float64 {
			return 1
		},
	)
	p.mustRegister(p.version)
}

func (p *Prometheus) getRegistererAndGatherer() (prometheus.Registerer, prometheus.Gatherer) {
	if p.Registry == nil {
		return prometheus.DefaultRegisterer, prometheus.DefaultGatherer
	}
	return p.Registry, p.Registry
}

func (p *Prometheus) register() {
	p.reqCnt = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: p.Namespace,
			Subsystem: p.Subsystem,
			Name:      p.RequestCounterMetricName,
			Help:      "How many HTTP requests processed, partitioned by status code and HTTP method.",
		},
		[]string{"code", "method", "handler", "host", "path"},
	)
	p.mustRegister(p.reqCnt)

	p.upTime = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: p.Namespace,
			Subsystem: p.Subsystem,
			Name:      p.UpTimeMetricName,
			Help:      "System Up-Time.",
		}, nil,
	)
	p.mustRegister(p.upTime)

	p.reqDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: p.Namespace,
		Subsystem: p.Subsystem,
		Buckets:   p.BucketsSize,
		Name:      p.RequestDurationMetricName,
		Help:      "The HTTP request latency bucket.",
	}, []string{"method", "path", "host"})
	p.mustRegister(p.reqDur)

	p.reqSz = prometheus.NewSummary(
		prometheus.SummaryOpts{
			Namespace: p.Namespace,
			Subsystem: p.Subsystem,
			Name:      p.RequestSizeMetricName,
			Help:      "The HTTP request sizes in bytes.",
		},
	)
	p.mustRegister(p.reqSz)

	p.resSz = prometheus.NewSummary(
		prometheus.SummaryOpts{
			Namespace: p.Namespace,
			Subsystem: p.Subsystem,
			Name:      p.ResponseSizeMetricName,
			Help:      "The HTTP response sizes in bytes.",
		},
	)
	p.mustRegister(p.resSz)
}

func (p *Prometheus) isIgnored(path string) bool {
	p.Ignored.RLock()
	defer p.Ignored.RUnlock()
	_, ok := p.Ignored.values[path]
	return ok
}

// recordUptime increases service uptime per second.
func (p *Prometheus) recordUptime() {
	for range time.Tick(time.Second) {
		p.upTime.WithLabelValues().Inc()
	}
}

// Instrument is a gin middleware that can be used to generate metrics for a
// single handler
func (p *Prometheus) Instrument() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := p.RequestPathFunc(c)

		if path == "" || p.isIgnored(path) {
			c.Next()
			return
		}

		reqSz := computeApproximateRequestSize(c.Request)

		c.Next()

		status := strconv.Itoa(c.Writer.Status())
		elapsed := float64(time.Since(start)) / float64(time.Second)
		resSz := float64(c.Writer.Size())

		p.reqCnt.WithLabelValues(status, c.Request.Method, p.HandlerNameFunc(c), c.Request.Host, path).Inc()
		p.reqDur.WithLabelValues(c.Request.Method, path, c.Request.Host).Observe(elapsed)
		p.reqSz.Observe(float64(reqSz))
		p.resSz.Observe(resSz)
	}
}

// Use is a method that should be used if the engine is set after middleware
// initialization.
func (p *Prometheus) Use(e *gin.Engine) {
	registerer, gatherer := p.getRegistererAndGatherer()
	e.GET(p.MetricsPath, prometheusHandler(p.Token, registerer, gatherer))
	p.Engine = e
}

func prometheusHandler(token string, registerer prometheus.Registerer, gatherer prometheus.Gatherer) gin.HandlerFunc {
	h := promhttp.InstrumentMetricHandler(
		registerer, promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}),
	)
	return func(c *gin.Context) {
		if token == "" {
			h.ServeHTTP(c.Writer, c.Request)
			return
		}

		header := c.Request.Header.Get("Authorization")

		if header == "" {
			c.String(http.StatusUnauthorized, ErrInvalidToken.Error())
			return
		}

		bearer := fmt.Sprintf("Bearer %s", token)

		if header != bearer {
			c.String(http.StatusUnauthorized, ErrInvalidToken.Error())
			return
		}

		h.ServeHTTP(c.Writer, c.Request)
	}
}
