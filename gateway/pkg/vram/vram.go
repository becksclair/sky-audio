package vram

import (
	"fmt"
	"sync"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

type Stats struct {
	DeviceIndex int
	TotalMB     uint64
	FreeMB      uint64
	UsedMB      uint64
}

var (
	initOnce sync.Once
	initErr  error
)

func ensureInit() error {
	initOnce.Do(func() {
		ret := nvml.Init()
		if ret != nvml.SUCCESS {
			initErr = fmt.Errorf("nvml init: %s", nvml.ErrorString(ret))
		}
	})
	return initErr
}

func Query(deviceIndex int) (Stats, error) {
	if err := ensureInit(); err != nil {
		return Stats{}, err
	}

	device, ret := nvml.DeviceGetHandleByIndex(deviceIndex)
	if ret != nvml.SUCCESS {
		return Stats{}, fmt.Errorf("nvml device %d: %s", deviceIndex, nvml.ErrorString(ret))
	}

	mem, ret := device.GetMemoryInfo()
	if ret != nvml.SUCCESS {
		return Stats{}, fmt.Errorf("nvml memory info: %s", nvml.ErrorString(ret))
	}

	return Stats{
		DeviceIndex: deviceIndex,
		TotalMB:     mem.Total / (1024 * 1024),
		FreeMB:      mem.Free / (1024 * 1024),
		UsedMB:      mem.Used / (1024 * 1024),
	}, nil
}
