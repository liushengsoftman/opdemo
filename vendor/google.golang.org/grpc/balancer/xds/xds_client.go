// +build go1.12

/*
 *
 * Copyright 2019 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package xds

import (
	"context"
	"sync"
	"time"

	xdspb "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	xdscorepb "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	xdsdiscoverypb "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v2"
	"github.com/gogo/protobuf/proto"
	"github.com/gogo/protobuf/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/internal/backoff"
	"google.golang.org/grpc/internal/channelz"
)

const (
	grpcHostname     = "com.googleapis.trafficdirector.grpc_hostname"
	cdsType          = "type.googleapis.com/envoy.api.v2.Cluster"
	edsType          = "type.googleapis.com/envoy.api.v2.ClusterLoadAssignment"
	endpointRequired = "endpoints_required"
)

var (
	defaultBackoffConfig = backoff.Exponential{
		MaxDelay: 120 * time.Second,
	}
)

// client is responsible for connecting to the specified traffic director, passing the received
// ADS response from the traffic director, and sending notification when communication with the
// traffic director is lost.
type client struct {
	ctx          context.Context
	cancel       context.CancelFunc
	cli          xdsdiscoverypb.AggregatedDiscoveryServiceClient
	opts         balancer.BuildOptions
	balancerName string // the traffic director name
	serviceName  string // the user dial target name
	enableCDS    bool
	newADS       func(ctx context.Context, resp proto.Message) error
	loseContact  func(ctx context.Context)
	cleanup      func()
	backoff      backoff.Strategy

	mu sync.Mutex
	cc *grpc.ClientConn
}

func (c *client) run() {
	c.dial()
	c.makeADSCall()
}

func (c *client) close() {
	c.cancel()
	c.mu.Lock()
	if c.cc != nil {
		c.cc.Close()
	}
	c.mu.Unlock()
	c.cleanup()
}

func (c *client) dial() {
	var dopts []grpc.DialOption
	if creds := c.opts.DialCreds; creds != nil {
		if err := creds.OverrideServerName(c.balancerName); err == nil {
			dopts = append(dopts, grpc.WithTransportCredentials(creds))
		} else {
			grpclog.Warningf("xds: failed to override the server name in the credentials: %v, using Insecure", err)
			dopts = append(dopts, grpc.WithInsecure())
		}
	} else {
		dopts = append(dopts, grpc.WithInsecure())
	}
	if c.opts.Dialer != nil {
		dopts = append(dopts, grpc.WithContextDialer(c.opts.Dialer))
	}
	// Explicitly set pickfirst as the balancer.
	dopts = append(dopts, grpc.WithBalancerName(grpc.PickFirstBalancerName))
	if channelz.IsOn() {
		dopts = append(dopts, grpc.WithChannelzParentID(c.opts.ChannelzParentID))
	}

	cc, err := grpc.DialContext(c.ctx, c.balancerName, dopts...)
	// Since this is a non-blocking dial, so if it fails, it due to some serious error (not network
	// related) error.
	if err != nil {
		grpclog.Fatalf("xds: failed to dial: %v", err)
	}
	c.mu.Lock()
	select {
	case <-c.ctx.Done():
		cc.Close()
	default:
		// only assign c.cc when xds client has not been closed, to prevent ClientConn leak.
		c.cc = cc
	}
	c.mu.Unlock()
}

func (c *client) newCDSRequest() *xdspb.DiscoveryRequest {
	cdsReq := &xdspb.DiscoveryRequest{
		Node: &xdscorepb.Node{
			Metadata: &types.Struct{
				Fields: map[string]*types.Value{
					grpcHostname: {
						Kind: &types.Value_StringValue{StringValue: c.serviceName},
					},
				},
			},
		},
		TypeUrl: cdsType,
	}
	return cdsReq
}

func (c *client) newEDSRequest() *xdspb.DiscoveryRequest {
	edsReq := &xdspb.DiscoveryRequest{
		Node: &xdscorepb.Node{
			Metadata: &types.Struct{
				Fields: map[string]*types.Value{
					endpointRequired: {
						Kind: &types.Value_BoolValue{BoolValue: c.enableCDS},
					},
				},
			},
		},
		ResourceNames: []string{c.serviceName},
		TypeUrl:       edsType,
	}
	return edsReq
}

func (c *client) makeADSCall() {
	c.cli = xdsdiscoverypb.NewAggregatedDiscoveryServiceClient(c.cc)
	retryCount := 0
	var doRetry bool

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		if doRetry {
			backoffTimer := time.NewTimer(c.backoff.Backoff(retryCount))
			select {
			case <-backoffTimer.C:
			case <-c.ctx.Done():
				backoffTimer.Stop()
				return
			}
			retryCount++
		}

		firstRespReceived := c.adsCallAttempt()
		if firstRespReceived {
			retryCount = 0
			doRetry = false
		} else {
			doRetry = true
		}
		c.loseContact(c.ctx)
	}
}

func (c *client) adsCallAttempt() (firstRespReceived bool) {
	firstRespReceived = false
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	st, err := c.cli.StreamAggregatedResources(ctx, grpc.WaitForReady(true))
	if err != nil {
		grpclog.Infof("xds: failed to initial ADS streaming RPC due to %v", err)
		return
	}
	if c.enableCDS {
		if err := st.Send(c.newCDSRequest()); err != nil {
			// current stream is broken, start a new one.
			return
		}
	}
	if err := st.Send(c.newEDSRequest()); err != nil {
		// current stream is broken, start a new one.
		return
	}
	expectCDS := c.enableCDS
	for {
		resp, err := st.Recv()
		if err != nil {
			// current stream is broken, start a new one.
			return
		}
		firstRespReceived = true
		resources := resp.GetResources()
		if len(resources) < 1 {
			grpclog.Warning("xds: ADS response contains 0 resource info.")
			// start a new call as server misbehaves by sending a ADS response with 0 resource info.
			return
		}
		if resp.GetTypeUrl() == cdsType && !c.enableCDS {
			grpclog.Warning("xds: received CDS response in custom plugin mode.")
			// start a new call as we receive CDS response when in EDS-only mode.
			return
		}
		var adsResp types.DynamicAny
		if err := types.UnmarshalAny(&resources[0], &adsResp); err != nil {
			grpclog.Warningf("xds: failed to unmarshal resources due to %v.", err)
			return
		}
		switch adsResp.Message.(type) {
		case *xdspb.Cluster:
			expectCDS = false
		case *xdspb.ClusterLoadAssignment:
			if expectCDS {
				grpclog.Warningf("xds: expecting CDS response, got EDS response instead.")
				return
			}
		}
		if err := c.newADS(c.ctx, adsResp.Message); err != nil {
			grpclog.Warningf("xds: processing new ADS message failed due to %v.", err)
			return
		}
	}
}
func newXDSClient(balancerName string, serviceName string, enableCDS bool, opts balancer.BuildOptions, newADS func(context.Context, proto.Message) error, loseContact func(ctx context.Context), exitCleanup func()) *client {
	c := &client{
		balancerName: balancerName,
		serviceName:  serviceName,
		enableCDS:    enableCDS,
		opts:         opts,
		newADS:       newADS,
		loseContact:  loseContact,
		cleanup:      exitCleanup,
		backoff:      defaultBackoffConfig,
	}

	c.ctx, c.cancel = context.WithCancel(context.Background())

	return c
}
