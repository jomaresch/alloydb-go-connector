// Copyright 2021 Google LLC
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

package alloydbconn

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	alloydbadmin "cloud.google.com/go/alloydb/apiv1beta"
	"cloud.google.com/go/alloydbconn/errtype"
	"cloud.google.com/go/alloydbconn/internal/alloydb"
	"cloud.google.com/go/alloydbconn/internal/mock"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
)

type stubTokenSource struct{}

func (stubTokenSource) Token() (*oauth2.Token, error) {
	return &oauth2.Token{}, nil
}

func TestDialerCanConnectToInstance(t *testing.T) {
	ctx := context.Background()
	inst := mock.NewFakeInstance(
		"my-project", "my-region", "my-cluster", "my-instance",
	)
	mc, url, cleanup := mock.HTTPClient(
		mock.InstanceGetSuccess(inst, 1),
		mock.CreateEphemeralSuccess(inst, 1),
	)
	stop := mock.StartServerProxy(t, inst)
	defer func() {
		stop()
		if err := cleanup(); err != nil {
			t.Fatalf("%v", err)
		}
	}()
	c, err := alloydbadmin.NewAlloyDBAdminRESTClient(
		ctx, option.WithHTTPClient(mc), option.WithEndpoint(url))
	if err != nil {
		t.Fatalf("expected NewClient to succeed, but got error: %v", err)
	}

	d, err := NewDialer(ctx, WithTokenSource(stubTokenSource{}))
	if err != nil {
		t.Fatalf("expected NewDialer to succeed, but got error: %v", err)
	}
	d.client = c

	// Run several tests to ensure the underlying shared buffer is properly
	// reset between connections.
	for i := 0; i < 10; i++ {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			conn, err := d.Dial(ctx, "projects/my-project/locations/my-region/clusters/my-cluster/instances/my-instance")
			if err != nil {
				t.Fatalf("expected Dial to succeed, but got error: %v", err)
			}
			defer conn.Close()
			data, err := io.ReadAll(conn)
			if err != nil {
				t.Fatalf("expected ReadAll to succeed, got error %v", err)
			}
			if string(data) != "my-instance" {
				t.Fatalf("expected known response from the server, but got %v", string(data))
			}
		})
	}

}

func TestDialWithAdminAPIErrors(t *testing.T) {
	ctx := context.Background()
	mc, url, cleanup := mock.HTTPClient()
	defer func() {
		if err := cleanup(); err != nil {
			t.Fatalf("%v", err)
		}
	}()
	c, err := alloydbadmin.NewAlloyDBAdminRESTClient(ctx, option.WithHTTPClient(mc), option.WithEndpoint(url))
	if err != nil {
		t.Fatalf("expected NewClient to succeed, but got error: %v", err)
	}
	d, err := NewDialer(ctx, WithTokenSource(stubTokenSource{}))
	if err != nil {
		t.Fatalf("expected NewDialer to succeed, but got error: %v", err)
	}
	d.client = c

	_, err = d.Dial(ctx, "bad-instance-name")
	var wantErr1 *errtype.ConfigError
	if !errors.As(err, &wantErr1) {
		t.Fatalf("when instance name is invalid, want = %T, got = %v", wantErr1, err)
	}

	ctx, cancel := context.WithCancel(ctx)
	cancel()

	_, err = d.Dial(ctx, "/projects/my-project/locations/my-region/clusters/my-cluster/instances/my-instance")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("when context is canceled, want = %T, got = %v", context.Canceled, err)
	}

	_, err = d.Dial(context.Background(), "/projects/my-project/locations/my-region/clusters/my-cluster/instances/my-instance")
	var wantErr2 *errtype.RefreshError
	if !errors.As(err, &wantErr2) {
		t.Fatalf("when API call fails, want = %T, got = %v", wantErr2, err)
	}
}

func TestDialWithUnavailableServerErrors(t *testing.T) {
	ctx := context.Background()
	inst := mock.NewFakeInstance(
		"my-project", "my-region", "my-cluster", "my-instance",
	)
	// Don't use the cleanup function. Because this test is about error
	// cases, API requests (started in two separate goroutines) will
	// sometimes succeed and clear the mock, and sometimes not.
	// This test is about error return values from Dial, not API interaction.
	mc, url, _ := mock.HTTPClient(
		mock.InstanceGetSuccess(inst, 2),
		mock.CreateEphemeralSuccess(inst, 2),
	)
	c, err := alloydbadmin.NewAlloyDBAdminRESTClient(ctx, option.WithHTTPClient(mc), option.WithEndpoint(url))
	if err != nil {
		t.Fatalf("expected NewClient to succeed, but got error: %v", err)
	}

	d, err := NewDialer(ctx, WithTokenSource(stubTokenSource{}))
	if err != nil {
		t.Fatalf("expected NewDialer to succeed, but got error: %v", err)
	}
	d.client = c

	_, err = d.Dial(ctx, "/projects/my-project/locations/my-region/clusters/my-cluster/instances/my-instance")
	var wantErr2 *errtype.DialError
	if !errors.As(err, &wantErr2) {
		t.Fatalf("when server proxy socket is unavailable, want = %T, got = %v", wantErr2, err)
	}
}

func TestDialerWithCustomDialFunc(t *testing.T) {
	ctx := context.Background()
	inst := mock.NewFakeInstance(
		"my-project", "my-region", "my-cluster", "my-instance",
	)
	mc, url, cleanup := mock.HTTPClient(
		mock.InstanceGetSuccess(inst, 1),
		mock.CreateEphemeralSuccess(inst, 1),
	)
	stop := mock.StartServerProxy(t, inst)
	defer func() {
		stop()
		if err := cleanup(); err != nil {
			t.Fatalf("%v", err)
		}
	}()
	c, err := alloydbadmin.NewAlloyDBAdminRESTClient(ctx, option.WithHTTPClient(mc), option.WithEndpoint(url))
	if err != nil {
		t.Fatalf("expected NewClient to succeed, but got error: %v", err)
	}

	d, err := NewDialer(ctx,
		WithDialFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
			return nil, errors.New("sentinel error")
		}),
		WithTokenSource(stubTokenSource{}),
	)
	if err != nil {
		t.Fatalf("expected NewDialer to succeed, but got error: %v", err)
	}
	d.client = c

	_, err = d.Dial(ctx, "/projects/my-project/locations/my-region/clusters/my-cluster/instances/my-instance")
	if !strings.Contains(err.Error(), "sentinel error") {
		t.Fatalf("want = sentinel error, got = %v", err)
	}
}

func TestDialerUserAgent(t *testing.T) {
	data, err := os.ReadFile("version.txt")
	if err != nil {
		t.Fatalf("failed to read version.txt: %v", err)
	}
	ver := strings.TrimSpace(string(data))
	want := "alloydb-go-connector/" + ver
	if want != userAgent {
		t.Errorf("embed version mismatched: want %q, got %q", want, userAgent)
	}
}

func TestDialerRemovesInvalidInstancesFromCache(t *testing.T) {
	// When a dialer attempts to retrieve connection info for a
	// non-existent instance, it should delete the instance from
	// the cache and ensure no background refresh happens (which would be
	// wasted cycles).
	d, err := NewDialer(context.Background(),
		WithTokenSource(stubTokenSource{}),
		WithRefreshTimeout(time.Second),
	)
	if err != nil {
		t.Fatalf("expected NewDialer to succeed, but got error: %v", err)
	}
	defer func(d *Dialer) {
		err := d.Close()
		if err != nil {
			t.Log(err)
		}
	}(d)

	// Populate instance map with connection info cache that will always fail.
	// This allows the test to verify the error case path invoking close.
	badInstanceName := "/projects/bad/locations/bad/clusters/bad/instances/bad"
	_, _ = d.Dial(context.Background(), badInstanceName)
	// The internal cache is not revealed publicly, so check the internal cache
	// to confirm the instance is no longer present.
	badInst, _ := alloydb.ParseInstURI(badInstanceName)

	spy := &spyConnectionInfoCache{
		connectInfoCalls: []struct {
			tls *tls.Config
			err error
		}{{
			err: errors.New("connect info failed"),
		}},
	}
	d.instances[badInst] = spy

	_, err = d.Dial(context.Background(), badInstanceName)
	if err == nil {
		t.Fatal("expected Dial to return error")
	}
	// Verify that the connection info cache was closed (to prevent
	// further failed refresh operations)
	if got, want := spy.CloseWasCalled(), true; got != want {
		t.Fatal("Close was not called")
	}

	// Now verify that bad connection name has been deleted from map.
	d.lock.RLock()
	_, ok := d.instances[badInst]
	d.lock.RUnlock()
	if ok {
		t.Fatal("bad instance was not removed from the cache")
	}
}

func TestDialRefreshesExpiredCertificates(t *testing.T) {
	d, err := NewDialer(
		context.Background(),
		WithTokenSource(stubTokenSource{}),
	)
	if err != nil {
		t.Fatalf("expected NewDialer to succeed, but got error: %v", err)
	}

	sentinel := errors.New("connect info failed")
	inst := "/projects/my-project/locations/my-region/clusters/my-cluster/instances/my-instance"
	cn, _ := alloydb.ParseInstURI(inst)
	spy := &spyConnectionInfoCache{
		connectInfoCalls: []struct {
			tls *tls.Config
			err error
		}{
			// First call returns expired certificate
			{
				tls: &tls.Config{
					Certificates: []tls.Certificate{{
						Leaf: &x509.Certificate{
							// Certificate expired 10 hours ago.
							NotAfter: time.Now().Add(-10 * time.Hour),
						},
					}},
				},
			},
			// Second call errors to validate error path
			{
				err: sentinel,
			},
		},
	}
	d.instances[cn] = spy

	_, err = d.Dial(context.Background(), inst)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected Dial to return sentinel error, instead got = %v", err)
	}

	// Verify that the cache was refreshed
	if got, want := spy.ForceRefreshWasCalled(), true; got != want {
		t.Fatal("ForceRefresh was not called")
	}

	// Verify that the connection info cache was closed (to prevent
	// further failed refresh operations)
	if got, want := spy.CloseWasCalled(), true; got != want {
		t.Fatal("Close was not called")
	}

	// Now verify that bad connection name has been deleted from map.
	d.lock.RLock()
	_, ok := d.instances[cn]
	d.lock.RUnlock()
	if ok {
		t.Fatal("bad instance was not removed from the cache")
	}
}

type spyConnectionInfoCache struct {
	mu               sync.Mutex
	connectInfoIndex int
	connectInfoCalls []struct {
		tls *tls.Config
		err error
	}
	closeWasCalled        bool
	forceRefreshWasCalled bool
	// embed interface to avoid having to implement irrelevant methods
	connectionInfoCache
}

func (s *spyConnectionInfoCache) ConnectInfo(_ context.Context) (string, *tls.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := s.connectInfoCalls[s.connectInfoIndex]
	s.connectInfoIndex++
	return "unused", res.tls, res.err
}

func (s *spyConnectionInfoCache) ForceRefresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forceRefreshWasCalled = true
}

func (s *spyConnectionInfoCache) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeWasCalled = true
	return nil
}

func (s *spyConnectionInfoCache) CloseWasCalled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeWasCalled
}

func (s *spyConnectionInfoCache) ForceRefreshWasCalled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forceRefreshWasCalled
}

func TestDialerSupportsOneOffDialFunction(t *testing.T) {
	ctx := context.Background()
	inst := mock.NewFakeInstance(
		"my-project", "my-region", "my-cluster", "my-instance",
	)
	mc, url, cleanup := mock.HTTPClient(
		mock.InstanceGetSuccess(inst, 1),
		mock.CreateEphemeralSuccess(inst, 1),
	)
	stop := mock.StartServerProxy(t, inst)
	defer func() {
		stop()
		if err := cleanup(); err != nil {
			t.Fatalf("%v", err)
		}
	}()
	c, err := alloydbadmin.NewAlloyDBAdminRESTClient(ctx, option.WithHTTPClient(mc), option.WithEndpoint(url))
	if err != nil {
		t.Fatalf("expected NewClient to succeed, but got error: %v", err)
	}

	d, err := NewDialer(ctx,
		WithDialFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
			return nil, errors.New("sentinel error")
		}),
		WithTokenSource(stubTokenSource{}),
	)
	if err != nil {
		t.Fatalf("expected NewDialer to succeed, but got error: %v", err)
	}
	d.client = c
	defer func() {
		if err := d.Close(); err != nil {
			t.Log(err)
		}
		_ = cleanup()
	}()

	sentinelErr := errors.New("dial func was called")
	f := func(context.Context, string, string) (net.Conn, error) {
		return nil, sentinelErr
	}

	_, err = d.Dial(ctx, "/projects/my-project/locations/my-region/clusters/my-cluster/instances/my-instance", WithOneOffDialFunc(f))
	if !errors.Is(err, sentinelErr) {
		t.Fatal("one-off dial func was not called")
	}
}
