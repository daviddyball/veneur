package datadog

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/sirupsen/logrus"
	"github.com/stripe/veneur/samplers"
	"github.com/stripe/veneur/trace"
)

const DatadogResourceKey = "resource"

type DatadogMetricSink struct {
	HTTPClient      *http.Client
	ddHostname      string
	hostname        string
	apiKey          string
	flushMaxPerBody int
	statsd          *statsd.Client
	tags            []string
	interval        float64
	traceClient     *trace.Client
	log             *logrus.Logger
}

// DDMetric is a data structure that represents the JSON that Datadog
// wants when posting to the API
type DDMetric struct {
	Name       string        `json:"metric"`
	Value      [1][2]float64 `json:"points"`
	Tags       []string      `json:"tags,omitempty"`
	MetricType string        `json:"type"`
	Hostname   string        `json:"host,omitempty"`
	DeviceName string        `json:"device_name,omitempty"`
	Interval   int32         `json:"interval,omitempty"`
}

// NewDatadogMetricSink creates a new Datadog sink for trace spans.
func NewDatadogMetricSink(interval float64, flushMaxPerBody int, hostname string, tags []string, ddHostname string, apiKey string, httpClient *http.Client, stats *statsd.Client, log *logrus.Logger) (*DatadogMetricSink, error) {
	return &DatadogMetricSink{
		HTTPClient:      httpClient,
		statsd:          stats,
		interval:        interval,
		flushMaxPerBody: flushMaxPerBody,
		hostname:        hostname,
		tags:            tags,
		ddHostname:      ddHostname,
		apiKey:          apiKey,
	}, nil
}

// Name returns the name of this sink.
func (dd *DatadogMetricSink) Name() string {
	return "datadog"
}

// Start sets the sink up.
func (dd *DatadogMetricSink) Start(cl *trace.Client) error {
	dd.traceClient = cl
	return nil
}

func (dd *DatadogMetricSink) Flush(ctx context.Context, interMetrics []samplers.InterMetric) error {
	span, _ := trace.StartSpanFromContext(ctx, "")
	defer span.ClientFinish(dd.traceClient)

	metrics := dd.finalizeMetrics(interMetrics)

	// break the metrics into chunks of approximately equal size, such that
	// each chunk is less than the limit
	// we compute the chunks using rounding-up integer division
	workers := ((len(metrics) - 1) / dd.flushMaxPerBody) + 1
	chunkSize := ((len(metrics) - 1) / workers) + 1
	dd.log.WithField("workers", workers).Debug("Worker count chosen")
	dd.log.WithField("chunkSize", chunkSize).Debug("Chunk size chosen")
	var wg sync.WaitGroup
	flushStart := time.Now()
	for i := 0; i < workers; i++ {
		chunk := metrics[i*chunkSize:]
		if i < workers-1 {
			// trim to chunk size unless this is the last one
			chunk = chunk[:chunkSize]
		}
		wg.Add(1)
		go dd.flushPart(span.Attach(ctx), chunk, &wg)
	}
	wg.Wait()
	dd.statsd.TimeInMilliseconds("flush.total_duration_ns", float64(time.Since(flushStart).Nanoseconds()), []string{"part:post"}, 1.0)

	dd.log.WithField("metrics", len(metrics)).Info("Completed flush to Datadog")
	return nil
}

func (dd *DatadogMetricSink) FlushEventsChecks(ctx context.Context, events []samplers.UDPEvent, checks []samplers.UDPServiceCheck) {
	span, _ := trace.StartSpanFromContext(ctx, "")
	defer span.ClientFinish(dd.traceClient)

	// fill in the default hostname for packets that didn't set it
	for i := range events {
		if events[i].Hostname == "" {
			events[i].Hostname = dd.hostname
		}
		events[i].Tags = append(events[i].Tags, dd.tags...)
	}
	for i := range checks {
		if checks[i].Hostname == "" {
			checks[i].Hostname = dd.hostname
		}
		checks[i].Tags = append(checks[i].Tags, dd.tags...)
	}

	if len(events) != 0 {
		// this endpoint is not documented at all, its existence is only known from
		// the official dd-agent
		// we don't actually pass all the body keys that dd-agent passes here... but
		// it still works
		err := postHelper(context.TODO(), dd.HTTPClient, dd.statsd, dd.traceClient, fmt.Sprintf("%s/intake?api_key=%s", dd.ddHostname, dd.apiKey), map[string]map[string][]samplers.UDPEvent{
			"events": {
				"api": events,
			},
		}, "flush_events", true)
		if err == nil {
			dd.log.WithField("events", len(events)).Info("Completed flushing events to Datadog")
		} else {
			dd.log.WithFields(logrus.Fields{
				"events":        len(events),
				logrus.ErrorKey: err}).Warn("Error flushing events to Datadog")
		}
	}

	if len(checks) != 0 {
		// this endpoint is not documented to take an array... but it does
		// another curious constraint of this endpoint is that it does not
		// support "Content-Encoding: deflate"
		err := postHelper(context.TODO(), dd.HTTPClient, dd.statsd, dd.traceClient, fmt.Sprintf("%s/api/v1/check_run?api_key=%s", dd.ddHostname, dd.apiKey), checks, "flush_checks", false)
		if err == nil {
			dd.log.WithField("checks", len(checks)).Info("Completed flushing service checks to Datadog")
		} else {
			dd.log.WithFields(logrus.Fields{
				"checks":        len(checks),
				logrus.ErrorKey: err}).Warn("Error flushing checks to Datadog")
		}
	}
}

func (dd *DatadogMetricSink) finalizeMetrics(metrics []samplers.InterMetric) []DDMetric {
	ddMetrics := make([]DDMetric, len(metrics))
	for i, m := range metrics {
		// Defensively copy tags since we're gonna mutate it
		tags := make([]string, len(dd.tags))
		copy(tags, dd.tags)

		metricType := ""
		value := m.Value

		switch m.Type {
		case samplers.CounterMetric:
			// We convert counters into rates for Datadog
			metricType = "rate"
			value = m.Value / dd.interval
		case samplers.GaugeMetric:
			metricType = "gauge"
		default:
			dd.log.WithField("metric_type", m.Type).Warn("Encountered an unknown metric type")
			continue
		}

		ddMetric := DDMetric{
			Name: m.Name,
			Value: [1][2]float64{
				[2]float64{
					float64(m.Timestamp), value,
				},
			},
			Tags:       tags,
			MetricType: metricType,
			Interval:   int32(dd.interval),
		}

		// Let's look for "magic tags" that override metric fields host and device.
		for _, tag := range m.Tags {
			// This overrides hostname
			if strings.HasPrefix(tag, "host:") {
				// Override the hostname with the tag, trimming off the prefix.
				ddMetric.Hostname = tag[5:]
			} else if strings.HasPrefix(tag, "device:") {
				// Same as above, but device this time
				ddMetric.DeviceName = tag[7:]
			} else {
				// Add it, no reason to exclude it.
				ddMetric.Tags = append(ddMetric.Tags, tag)
			}
		}
		if ddMetric.Hostname == "" {
			// No magic tag, set the hostname
			ddMetric.Hostname = dd.hostname
		}
		ddMetrics[i] = ddMetric
	}

	return ddMetrics
}

func (dd *DatadogMetricSink) flushPart(ctx context.Context, metricSlice []DDMetric, wg *sync.WaitGroup) {
	defer wg.Done()
	postHelper(ctx, dd.HTTPClient, dd.statsd, dd.traceClient, fmt.Sprintf("%s/api/v1/series?api_key=%s", dd.ddHostname, dd.apiKey), map[string][]DDMetric{
		"series": metricSlice,
	}, "flush", true)
}
