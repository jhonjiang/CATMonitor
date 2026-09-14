//go:build !linux || !cgo

package npu_dvfs

// stubSource is the Source used when the CGo dlopen binding is not compiled
// in (non-Linux platforms, or CGo disabled). Every call fails with
// errNotAvailable; Available() reports false so callers degrade gracefully.
type stubSource struct{}

var defaultSrc Source = stubSource{}

func (stubSource) Available() bool { return false }

func (stubSource) DeviceIDs() ([]int, error) { return nil, errNotAvailable }

func (stubSource) RatedFreq(devID int) (int, error) { return 0, errNotAvailable }

func (stubSource) CloseIdle(devID int) error { return errNotAvailable }

func (stubSource) OpenIdle(devID int) error { return errNotAvailable }

func (stubSource) SetAicFreq(devID int, freqMHz int) error { return errNotAvailable }
