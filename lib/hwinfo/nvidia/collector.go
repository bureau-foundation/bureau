// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package nvidia

import (
	"log/slog"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// Collector is a stub implementation of hwinfo.GPUCollector for NVIDIA
// GPUs. Dynamic GPU metrics (utilization, temperature, power, clocks)
// are not available without NVML (libnvidia-ml.so), which requires
// either cgo or purego-style dlopen — neither of which Bureau uses
// today. This stub returns nil from Collect() so that NVIDIA GPUs
// appear in static inventory (via the Prober) but report no dynamic
// metrics in heartbeats.
type Collector struct {
	logger *slog.Logger
}

// NewCollector creates a stub Collector. It logs once at Info level
// to document that NVIDIA dynamic metrics are not available.
func NewCollector(logger *slog.Logger) *Collector {
	logger.Info("nvidia collector: dynamic GPU metrics not available (requires NVML)")
	return &Collector{logger: logger}
}

// Collect returns nil — NVIDIA dynamic metrics are not available
// without NVML.
func (c *Collector) Collect() []schema.GPUStatus {
	return nil
}

// Close is a no-op for the stub collector.
func (c *Collector) Close() {}
