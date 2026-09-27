package infra

import (
	"sync"
	"time"
)

type SharedLeaseStatus struct {
	SharedLeaseRecord
	State        string        `json:"state"`
	Age          time.Duration `json:"age"`
	HeartbeatAge time.Duration `json:"heartbeat_age"`
}

// SharedRuntimeLease keeps the broker connection that owns a process-lifetime
// lease. Closing it releases the lease even if the broker cannot be reached.
type SharedRuntimeLease struct {
	record        SharedLeaseRecord
	monitor       func() <-chan error
	close         func()
	monitorOnce   sync.Once
	monitorResult <-chan error
}

func (lease *SharedRuntimeLease) Record() SharedLeaseRecord {
	if lease == nil {
		return SharedLeaseRecord{}
	}
	return lease.record
}

func (lease *SharedRuntimeLease) Monitor() <-chan error {
	if lease == nil || lease.monitor == nil {
		closed := make(chan error)
		close(closed)
		return closed
	}
	lease.monitorOnce.Do(func() { lease.monitorResult = lease.monitor() })
	return lease.monitorResult
}

func (lease *SharedRuntimeLease) Close() {
	if lease != nil && lease.close != nil {
		lease.close()
	}
}
