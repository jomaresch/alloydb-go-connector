// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package alloydb

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"fmt"
	"regexp"
	"sync"
	"time"

	alloydbadmin "cloud.google.com/go/alloydb/apiv1beta"
	"cloud.google.com/go/alloydbconn/errtype"
	"golang.org/x/time/rate"
)

const (
	// the refresh buffer is the amount of time before a refresh cycle's result
	// expires that a new refresh operation begins.
	refreshBuffer = 4 * time.Minute

	// refreshInterval is the amount of time between refresh attempts as
	// enforced by the rate limiter.
	refreshInterval = 30 * time.Second

	// RefreshTimeout is the maximum amount of time to wait for a refresh
	// cycle to complete. This value should be greater than the
	// refreshInterval.
	RefreshTimeout = 60 * time.Second

	// refreshBurst is the initial burst allowed by the rate limiter.
	refreshBurst = 2
)

var (
	// Instance URI is in the format:
	// '/projects/<PROJECT>/locations/<REGION>/clusters/<CLUSTER>/instances/<INSTANCE>'
	// Additionally, we have to support legacy "domain-scoped" projects
	// (e.g. "google.com:PROJECT")
	instURIRegex = regexp.MustCompile("projects/([^:]+(:[^:]+)?)/locations/([^:]+)/clusters/([^:]+)/instances/([^:]+)")
)

// InstanceURI represents an AlloyDB instance.
type InstanceURI struct {
	project string
	region  string
	cluster string
	name    string
}

func (i *InstanceURI) String() string {
	return fmt.Sprintf("%s/%s/%s/%s", i.project, i.region, i.cluster, i.name)
}

// ParseInstURI initializes a new InstanceURI struct.
func ParseInstURI(cn string) (InstanceURI, error) {
	b := []byte(cn)
	m := instURIRegex.FindSubmatch(b)
	if m == nil {
		err := errtype.NewConfigError(
			"invalid instance URI, expected projects/<PROJECT>/locations/<REGION>/clusters/<CLUSTER>/instances/<INSTANCE>",
			cn,
		)
		return InstanceURI{}, err
	}

	c := InstanceURI{
		project: string(m[1]),
		region:  string(m[3]),
		cluster: string(m[4]),
		name:    string(m[5]),
	}
	return c, nil
}

// refreshOperation is a pending result of a refresh operation of data used to
// connect securely. It should only be initialized by the Instance struct as
// part of a refresh cycle.
type refreshOperation struct {
	result refreshResult
	err    error

	// timer that triggers refresh, can be used to cancel.
	timer *time.Timer
	// indicates the struct is ready to read from
	ready chan struct{}
}

// Cancel prevents the instanceInfo from starting, if it hasn't already
// started. Returns true if timer was stopped successfully, or false if it has
// already started.
func (r *refreshOperation) cancel() bool {
	return r.timer.Stop()
}

// IsValid returns true if this result is complete, successful, and is still
// valid.
func (r *refreshOperation) isValid() bool {
	// verify the result has finished running
	select {
	default:
		return false
	case <-r.ready:
		if r.err != nil || time.Now().After(r.result.expiry) {
			return false
		}
		return true
	}
}

// Instance manages the information used to connect to the AlloyDB instance by
// periodically calling the AlloyDB Admin API. It automatically refreshes the
// required information approximately 4 minutes before the previous certificate
// expires (every ~56 minutes).
type Instance struct {
	// OpenConns is the number of open connections to the instance.
	openConns uint64

	instanceURI InstanceURI
	key         *rsa.PrivateKey
	// refreshTimeout sets the maximum duration a refresh cycle can run
	// for.
	refreshTimeout time.Duration
	// l controls the rate at which refresh cycles are run.
	l *rate.Limiter
	r refresher

	resultGuard sync.RWMutex
	// cur represents the current refreshOperation that will be used to
	// create connections. If a valid complete refreshOperation isn't
	// available it's possible for cur to be equal to next.
	cur *refreshOperation
	// next represents a future or ongoing refreshOperation. Once complete,
	// it will replace cur and schedule a replacement to occur.
	next *refreshOperation

	// ctx is the default ctx for refresh operations. Canceling it prevents
	// new refresh operations from being triggered.
	ctx    context.Context
	cancel context.CancelFunc
}

// NewInstance initializes a new Instance given an instance URI
func NewInstance(
	instance InstanceURI,
	client *alloydbadmin.AlloyDBAdminClient,
	key *rsa.PrivateKey,
	refreshTimeout time.Duration,
	dialerID string,
) *Instance {
	ctx, cancel := context.WithCancel(context.Background())
	i := &Instance{
		instanceURI:    instance,
		key:            key,
		l:              rate.NewLimiter(rate.Every(refreshInterval), refreshBurst),
		r:              newRefresher(client, dialerID),
		refreshTimeout: refreshTimeout,
		ctx:            ctx,
		cancel:         cancel,
	}
	// For the initial refresh operation, set cur = next so that connection
	// requests block until the first refresh is complete.
	i.resultGuard.Lock()
	i.cur = i.scheduleRefresh(0)
	i.next = i.cur
	i.resultGuard.Unlock()
	return i
}

// OpenConns reports the number of open connections.
func (i *Instance) OpenConns() *uint64 {
	return &i.openConns
}

// Close closes the instance; it stops the refresh cycle and prevents it from
// making additional calls to the AlloyDB Admin API.
func (i *Instance) Close() error {
	i.cancel()
	return nil
}

// ConnectInfo returns an IP address of the AlloyDB instance.
func (i *Instance) ConnectInfo(ctx context.Context) (string, *tls.Config, error) {
	res, err := i.result(ctx)
	if err != nil {
		return "", nil, err
	}
	return res.result.instanceIPAddr, res.result.conf, nil
}

// ForceRefresh triggers an immediate refresh operation to be scheduled and
// used for future connection attempts if valid.
func (i *Instance) ForceRefresh() {
	i.resultGuard.Lock()
	defer i.resultGuard.Unlock()
	// If the next refresh hasn't started yet, we can cancel it and start an immediate one
	if i.next.cancel() {
		i.next = i.scheduleRefresh(0)
	}
	// block all sequential connection attempts on the next refresh operation
	// if current is invalid
	if !i.cur.isValid() {
		i.cur = i.next
	}
}

// result returns the most recent refresh result (waiting for it to complete if
// necessary)
func (i *Instance) result(ctx context.Context) (*refreshOperation, error) {
	i.resultGuard.RLock()
	res := i.cur
	i.resultGuard.RUnlock()
	var err error
	select {
	case <-res.ready:
		err = res.err
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// refreshDuration returns the duration to wait before starting the next
// refresh. Usually that duration will be half of the time until certificate
// expiration.
func refreshDuration(now, certExpiry time.Time) time.Duration {
	d := certExpiry.Sub(now)
	if d < time.Hour {
		// Something is wrong with the certification, refresh now.
		if d < refreshBuffer {
			return 0
		}
		// Otherwise wait until 4 minutes before expiration for next refresh cycle.
		return d - refreshBuffer
	}
	return d / 2
}

// scheduleRefresh schedules a refresh operation to be triggered after a given
// duration. The returned refreshOperation can be used to either Cancel or Wait
// for the operation's result.
func (i *Instance) scheduleRefresh(d time.Duration) *refreshOperation {
	r := &refreshOperation{}
	r.ready = make(chan struct{})
	r.timer = time.AfterFunc(d, func() {
		ctx, cancel := context.WithTimeout(i.ctx, i.refreshTimeout)
		defer cancel()

		err := i.l.Wait(ctx)
		if err != nil {
			r.err = errtype.NewDialError(
				"context was canceled or expired before refresh completed",
				i.instanceURI.String(),
				nil,
			)
		} else {
			r.result, r.err = i.r.performRefresh(i.ctx, i.instanceURI, i.key)
		}

		close(r.ready)

		// Once the refresh is complete, update "current" with working
		// result and schedule a new refresh
		i.resultGuard.Lock()
		defer i.resultGuard.Unlock()
		// if failed, scheduled the next refresh immediately
		if r.err != nil {
			i.next = i.scheduleRefresh(0)
			// If the latest result is bad, avoid replacing the
			// used result while it's still valid and potentially
			// able to provide successful connections. TODO: This
			// means that errors while the current result is still
			// valid are suppressed. We should try to surface
			// errors in a more meaningful way.
			if !i.cur.isValid() {
				i.cur = r
			}
			return
		}
		// Update the current results, and schedule the next refresh in
		// the future
		i.cur = r
		select {
		case <-i.ctx.Done():
			// instance has been closed, don't schedule anything
			return
		default:
		}
		t := refreshDuration(time.Now(), i.cur.result.expiry)
		i.next = i.scheduleRefresh(t)
	})
	return r
}
