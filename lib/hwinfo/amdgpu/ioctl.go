// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package amdgpu

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// DRM ioctl constants derived from the upstream Linux kernel UAPI headers
// (include/uapi/drm/amdgpu_drm.h). These are stable ABI — the kernel
// guarantees backward compatibility for UAPI ioctl interfaces.
const (
	// drmIoctlBase is the DRM ioctl type character ('d').
	drmIoctlBase = 'd'

	// drmCommandBase is the base number for driver-private ioctls.
	drmCommandBase = 0x40

	// drmAMDGPUInfo is the DRM_AMDGPU_INFO command number.
	drmAMDGPUInfo = 0x05

	// ioctlAMDGPUInfo is the fully encoded ioctl number for
	// DRM_IOCTL_AMDGPU_INFO. Encodes _IOW('d', 0x45, 64) where 64
	// is sizeof(struct drm_amdgpu_info).
	//
	// Bit layout: direction(1=write) << 30 | size(64) << 16 | type('d') << 8 | nr(0x45)
	ioctlAMDGPUInfo = 0x40406445
)

// AMDGPU_INFO query type constants from amdgpu_drm.h.
const (
	amdgpuInfoSensor = 0x1D
)

// AMDGPU_INFO_SENSOR sub-query constants.
const (
	// SensorGFXSCLK queries the current graphics clock in MHz.
	SensorGFXSCLK = 0x1

	// SensorGFXMCLK queries the current memory clock in MHz.
	SensorGFXMCLK = 0x2

	// SensorGPUTemp queries the GPU temperature in millidegrees Celsius.
	SensorGPUTemp = 0x3

	// SensorGPULoad queries the GPU utilization percentage (0-100).
	SensorGPULoad = 0x4

	// SensorGPUAvgPower queries the average GPU power draw in watts.
	SensorGPUAvgPower = 0x5
)

// drmAMDGPUInfoRequest mirrors struct drm_amdgpu_info from the kernel
// UAPI header. The struct is 64 bytes total: 8 (return_pointer) + 4
// (return_size) + 4 (query) + 48 (union data). For sensor queries,
// only the first 4 bytes of the union are used (sensor type).
type drmAMDGPUInfoRequest struct {
	returnPointer uint64
	returnSize    uint32
	query         uint32
	unionData     [48]byte
}

// querySensor issues a single AMDGPU_INFO_SENSOR ioctl on the given
// file descriptor (an open render node) and returns the uint32 result.
// The sensor type is one of the Sensor* constants above.
func querySensor(fd uintptr, sensorType uint32) (uint32, error) {
	var result uint32

	var request drmAMDGPUInfoRequest
	request.returnPointer = uint64(uintptr(unsafe.Pointer(&result)))
	request.returnSize = 4
	request.query = amdgpuInfoSensor
	// Write sensor type into the first 4 bytes of the union (little-endian).
	binary.LittleEndian.PutUint32(request.unionData[:4], sensorType)

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		fd,
		uintptr(ioctlAMDGPUInfo),
		uintptr(unsafe.Pointer(&request)),
	)
	if errno != 0 {
		return 0, fmt.Errorf("amdgpu ioctl sensor query 0x%x: %w", sensorType, errno)
	}
	return result, nil
}
