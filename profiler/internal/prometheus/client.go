package prometheus

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// PeakVRAMResult holds the result of a peak VRAM query for a pod.
// All label fields are populated directly from the Prometheus metric labels.
type PeakVRAMResult struct {
	PodName       string
	PodNamespace  string
	PodUID        string // from "pod_uid" label
	ContainerName string // from "ctrname" label
	DeviceUUID    string // from "deviceuuid" label
	DeviceType    string // from "device_type" label
	VDeviceID     string // from "vdeviceid" label
	Image         string // from "image" label (e.g. docker.io/deepspeed/deepspeed:latest)
	ImageID       string // from "image_id" label (e.g. docker.io/...@sha256:...)
	PeakValueMiB  float64
	MetricName    string
}

// Client wraps the Prometheus v1 API.
type Client struct {
	api v1.API
}

// NewClient creates a Prometheus query client.
func NewClient(address string) (*Client, error) {
	c, err := api.NewClient(api.Config{
		Address: address,
	})
	if err != nil {
		return nil, fmt.Errorf("creating prometheus client: %w", err)
	}
	return &Client{api: v1.NewAPI(c)}, nil
}

// QueryPeakVRAM queries Prometheus for the max VRAM usage of a pod over a
// given duration using max_over_time.
//
// Example query:
//
//	max_over_time(vGPU_device_memory_usage_real_in_MiB{podname="pod-a",podnamespace="ns"}[3600s])
func (c *Client) QueryPeakVRAM(ctx context.Context, metricName, podName, podNamespace string, duration time.Duration) ([]PeakVRAMResult, error) {
	durationSec := int(duration.Seconds())
	if durationSec < 1 {
		durationSec = 1
	}

	query := fmt.Sprintf(
		`max_over_time(%s{podname="%s",podnamespace="%s"}[%ds])`,
		metricName, podName, podNamespace, durationSec,
	)

	log.Printf("Prometheus query: %s", query)

	result, warnings, err := c.api.Query(ctx, query, time.Now(), v1.WithTimeout(15*time.Second))
	if err != nil {
		return nil, fmt.Errorf("querying prometheus: %w", err)
	}
	if len(warnings) > 0 {
		log.Printf("Warnings: %v", warnings)
	}

	vector, ok := result.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}

	if len(vector) == 0 {
		return nil, fmt.Errorf("no data returned for pod %s/%s over %ds", podNamespace, podName, durationSec)
	}

	var results []PeakVRAMResult
	for _, sample := range vector {
		r := PeakVRAMResult{
			PodName:       podName,
			PodNamespace:  podNamespace,
			PodUID:        string(sample.Metric["pod_uid"]),
			ContainerName: string(sample.Metric["ctrname"]),
			DeviceUUID:    string(sample.Metric["deviceuuid"]),
			DeviceType:    string(sample.Metric["device_type"]),
			VDeviceID:     string(sample.Metric["vdeviceid"]),
			Image:         string(sample.Metric["image"]),
			ImageID:       string(sample.Metric["image_id"]),
			PeakValueMiB:  float64(sample.Value),
			MetricName:    metricName,
		}
		results = append(results, r)
	}

	return results, nil
}
