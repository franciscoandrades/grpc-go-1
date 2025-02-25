/*
 *
 * Copyright 2021 gRPC authors.
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

package xdsclient_test

import (
	"fmt"
	"testing"
	"time"

	v3clusterpb "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	v3corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	v3listenerpb "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	v3routepb "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	v3httppb "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/internal/testutils"
	xdstestutils "google.golang.org/grpc/xds/internal/testutils"
	"google.golang.org/grpc/xds/internal/xdsclient"
	"google.golang.org/grpc/xds/internal/xdsclient/bootstrap"
)

const defaultTestWatchExpiryTimeout = 500 * time.Millisecond

func (s) TestLDSConfigDump(t *testing.T) {
	const testVersion = "test-version-lds"
	var (
		ldsTargets       = []string{"lds.target.good:0000", "lds.target.good:1111"}
		routeConfigNames = []string{"route-config-0", "route-config-1"}
		listenerRaws     = make(map[string]*anypb.Any, len(ldsTargets))
	)

	for i := range ldsTargets {
		listenersT := &v3listenerpb.Listener{
			Name: ldsTargets[i],
			ApiListener: &v3listenerpb.ApiListener{
				ApiListener: testutils.MarshalAny(&v3httppb.HttpConnectionManager{
					RouteSpecifier: &v3httppb.HttpConnectionManager_Rds{
						Rds: &v3httppb.Rds{
							ConfigSource: &v3corepb.ConfigSource{
								ConfigSourceSpecifier: &v3corepb.ConfigSource_Ads{Ads: &v3corepb.AggregatedConfigSource{}},
							},
							RouteConfigName: routeConfigNames[i],
						},
					},
					CommonHttpProtocolOptions: &v3corepb.HttpProtocolOptions{
						MaxStreamDuration: durationpb.New(time.Second),
					},
				}),
			},
		}
		listenerRaws[ldsTargets[i]] = testutils.MarshalAny(listenersT)
	}

	client, err := xdsclient.NewWithConfigForTesting(&bootstrap.Config{
		XDSServer: &bootstrap.ServerConfig{
			ServerURI: testXDSServer,
			Creds:     grpc.WithTransportCredentials(insecure.NewCredentials()),
			NodeProto: xdstestutils.EmptyNodeProtoV2,
		},
	}, defaultTestWatchExpiryTimeout)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	defer client.Close()
	updateHandler := client.(xdsclient.UpdateHandler)

	// Expected unknown.
	if err := compareDump(client.DumpLDS, "", map[string]xdsclient.UpdateWithMD{}); err != nil {
		t.Fatalf(err.Error())
	}

	wantRequested := make(map[string]xdsclient.UpdateWithMD)
	for _, n := range ldsTargets {
		cancel := client.WatchListener(n, func(update xdsclient.ListenerUpdate, err error) {})
		defer cancel()
		wantRequested[n] = xdsclient.UpdateWithMD{MD: xdsclient.UpdateMetadata{Status: xdsclient.ServiceStatusRequested}}
	}
	// Expected requested.
	if err := compareDump(client.DumpLDS, "", wantRequested); err != nil {
		t.Fatalf(err.Error())
	}

	update0 := make(map[string]xdsclient.ListenerUpdateErrTuple)
	want0 := make(map[string]xdsclient.UpdateWithMD)
	for n, r := range listenerRaws {
		update0[n] = xdsclient.ListenerUpdateErrTuple{Update: xdsclient.ListenerUpdate{Raw: r}}
		want0[n] = xdsclient.UpdateWithMD{
			MD:  xdsclient.UpdateMetadata{Status: xdsclient.ServiceStatusACKed, Version: testVersion},
			Raw: r,
		}
	}
	updateHandler.NewListeners(update0, xdsclient.UpdateMetadata{Status: xdsclient.ServiceStatusACKed, Version: testVersion})

	// Expect ACK.
	if err := compareDump(client.DumpLDS, testVersion, want0); err != nil {
		t.Fatalf(err.Error())
	}

	const nackVersion = "lds-version-nack"
	var nackErr = fmt.Errorf("lds nack error")
	updateHandler.NewListeners(
		map[string]xdsclient.ListenerUpdateErrTuple{
			ldsTargets[0]: {Err: nackErr},
			ldsTargets[1]: {Update: xdsclient.ListenerUpdate{Raw: listenerRaws[ldsTargets[1]]}},
		},
		xdsclient.UpdateMetadata{
			Status: xdsclient.ServiceStatusNACKed,
			ErrState: &xdsclient.UpdateErrorMetadata{
				Version: nackVersion,
				Err:     nackErr,
			},
		},
	)

	// Expect NACK for [0], but old ACK for [1].
	wantDump := make(map[string]xdsclient.UpdateWithMD)
	// Though resource 0 was NACKed, the dump should show the previous ACKed raw
	// message, as well as the NACK error.
	wantDump[ldsTargets[0]] = xdsclient.UpdateWithMD{
		MD: xdsclient.UpdateMetadata{
			Status:  xdsclient.ServiceStatusNACKed,
			Version: testVersion,
			ErrState: &xdsclient.UpdateErrorMetadata{
				Version: nackVersion,
				Err:     nackErr,
			},
		},
		Raw: listenerRaws[ldsTargets[0]],
	}

	wantDump[ldsTargets[1]] = xdsclient.UpdateWithMD{
		MD:  xdsclient.UpdateMetadata{Status: xdsclient.ServiceStatusACKed, Version: nackVersion},
		Raw: listenerRaws[ldsTargets[1]],
	}
	if err := compareDump(client.DumpLDS, nackVersion, wantDump); err != nil {
		t.Fatalf(err.Error())
	}
}

func (s) TestRDSConfigDump(t *testing.T) {
	const testVersion = "test-version-rds"
	var (
		listenerNames = []string{"lds.target.good:0000", "lds.target.good:1111"}
		rdsTargets    = []string{"route-config-0", "route-config-1"}
		clusterNames  = []string{"cluster-0", "cluster-1"}
		routeRaws     = make(map[string]*anypb.Any, len(rdsTargets))
	)

	for i := range rdsTargets {
		routeConfigT := &v3routepb.RouteConfiguration{
			Name: rdsTargets[i],
			VirtualHosts: []*v3routepb.VirtualHost{
				{
					Domains: []string{listenerNames[i]},
					Routes: []*v3routepb.Route{{
						Match: &v3routepb.RouteMatch{PathSpecifier: &v3routepb.RouteMatch_Prefix{Prefix: ""}},
						Action: &v3routepb.Route_Route{
							Route: &v3routepb.RouteAction{
								ClusterSpecifier: &v3routepb.RouteAction_Cluster{Cluster: clusterNames[i]},
							},
						},
					}},
				},
			},
		}

		routeRaws[rdsTargets[i]] = testutils.MarshalAny(routeConfigT)
	}

	client, err := xdsclient.NewWithConfigForTesting(&bootstrap.Config{
		XDSServer: &bootstrap.ServerConfig{
			ServerURI: testXDSServer,
			Creds:     grpc.WithTransportCredentials(insecure.NewCredentials()),
			NodeProto: xdstestutils.EmptyNodeProtoV2,
		},
	}, defaultTestWatchExpiryTimeout)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	defer client.Close()
	updateHandler := client.(xdsclient.UpdateHandler)

	// Expected unknown.
	if err := compareDump(client.DumpRDS, "", map[string]xdsclient.UpdateWithMD{}); err != nil {
		t.Fatalf(err.Error())
	}

	wantRequested := make(map[string]xdsclient.UpdateWithMD)
	for _, n := range rdsTargets {
		cancel := client.WatchRouteConfig(n, func(update xdsclient.RouteConfigUpdate, err error) {})
		defer cancel()
		wantRequested[n] = xdsclient.UpdateWithMD{MD: xdsclient.UpdateMetadata{Status: xdsclient.ServiceStatusRequested}}
	}
	// Expected requested.
	if err := compareDump(client.DumpRDS, "", wantRequested); err != nil {
		t.Fatalf(err.Error())
	}

	update0 := make(map[string]xdsclient.RouteConfigUpdateErrTuple)
	want0 := make(map[string]xdsclient.UpdateWithMD)
	for n, r := range routeRaws {
		update0[n] = xdsclient.RouteConfigUpdateErrTuple{Update: xdsclient.RouteConfigUpdate{Raw: r}}
		want0[n] = xdsclient.UpdateWithMD{
			MD:  xdsclient.UpdateMetadata{Status: xdsclient.ServiceStatusACKed, Version: testVersion},
			Raw: r,
		}
	}
	updateHandler.NewRouteConfigs(update0, xdsclient.UpdateMetadata{Status: xdsclient.ServiceStatusACKed, Version: testVersion})

	// Expect ACK.
	if err := compareDump(client.DumpRDS, testVersion, want0); err != nil {
		t.Fatalf(err.Error())
	}

	const nackVersion = "rds-version-nack"
	var nackErr = fmt.Errorf("rds nack error")
	updateHandler.NewRouteConfigs(
		map[string]xdsclient.RouteConfigUpdateErrTuple{
			rdsTargets[0]: {Err: nackErr},
			rdsTargets[1]: {Update: xdsclient.RouteConfigUpdate{Raw: routeRaws[rdsTargets[1]]}},
		},
		xdsclient.UpdateMetadata{
			Status: xdsclient.ServiceStatusNACKed,
			ErrState: &xdsclient.UpdateErrorMetadata{
				Version: nackVersion,
				Err:     nackErr,
			},
		},
	)

	// Expect NACK for [0], but old ACK for [1].
	wantDump := make(map[string]xdsclient.UpdateWithMD)
	// Though resource 0 was NACKed, the dump should show the previous ACKed raw
	// message, as well as the NACK error.
	wantDump[rdsTargets[0]] = xdsclient.UpdateWithMD{
		MD: xdsclient.UpdateMetadata{
			Status:  xdsclient.ServiceStatusNACKed,
			Version: testVersion,
			ErrState: &xdsclient.UpdateErrorMetadata{
				Version: nackVersion,
				Err:     nackErr,
			},
		},
		Raw: routeRaws[rdsTargets[0]],
	}
	wantDump[rdsTargets[1]] = xdsclient.UpdateWithMD{
		MD:  xdsclient.UpdateMetadata{Status: xdsclient.ServiceStatusACKed, Version: nackVersion},
		Raw: routeRaws[rdsTargets[1]],
	}
	if err := compareDump(client.DumpRDS, nackVersion, wantDump); err != nil {
		t.Fatalf(err.Error())
	}
}

func (s) TestCDSConfigDump(t *testing.T) {
	const testVersion = "test-version-cds"
	var (
		cdsTargets   = []string{"cluster-0", "cluster-1"}
		serviceNames = []string{"service-0", "service-1"}
		clusterRaws  = make(map[string]*anypb.Any, len(cdsTargets))
	)

	for i := range cdsTargets {
		clusterT := &v3clusterpb.Cluster{
			Name:                 cdsTargets[i],
			ClusterDiscoveryType: &v3clusterpb.Cluster_Type{Type: v3clusterpb.Cluster_EDS},
			EdsClusterConfig: &v3clusterpb.Cluster_EdsClusterConfig{
				EdsConfig: &v3corepb.ConfigSource{
					ConfigSourceSpecifier: &v3corepb.ConfigSource_Ads{
						Ads: &v3corepb.AggregatedConfigSource{},
					},
				},
				ServiceName: serviceNames[i],
			},
			LbPolicy: v3clusterpb.Cluster_ROUND_ROBIN,
			LrsServer: &v3corepb.ConfigSource{
				ConfigSourceSpecifier: &v3corepb.ConfigSource_Self{
					Self: &v3corepb.SelfConfigSource{},
				},
			},
		}

		clusterRaws[cdsTargets[i]] = testutils.MarshalAny(clusterT)
	}

	client, err := xdsclient.NewWithConfigForTesting(&bootstrap.Config{
		XDSServer: &bootstrap.ServerConfig{
			ServerURI: testXDSServer,
			Creds:     grpc.WithTransportCredentials(insecure.NewCredentials()),
			NodeProto: xdstestutils.EmptyNodeProtoV2,
		},
	}, defaultTestWatchExpiryTimeout)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	defer client.Close()
	updateHandler := client.(xdsclient.UpdateHandler)

	// Expected unknown.
	if err := compareDump(client.DumpCDS, "", map[string]xdsclient.UpdateWithMD{}); err != nil {
		t.Fatalf(err.Error())
	}

	wantRequested := make(map[string]xdsclient.UpdateWithMD)
	for _, n := range cdsTargets {
		cancel := client.WatchCluster(n, func(update xdsclient.ClusterUpdate, err error) {})
		defer cancel()
		wantRequested[n] = xdsclient.UpdateWithMD{MD: xdsclient.UpdateMetadata{Status: xdsclient.ServiceStatusRequested}}
	}
	// Expected requested.
	if err := compareDump(client.DumpCDS, "", wantRequested); err != nil {
		t.Fatalf(err.Error())
	}

	update0 := make(map[string]xdsclient.ClusterUpdateErrTuple)
	want0 := make(map[string]xdsclient.UpdateWithMD)
	for n, r := range clusterRaws {
		update0[n] = xdsclient.ClusterUpdateErrTuple{Update: xdsclient.ClusterUpdate{Raw: r}}
		want0[n] = xdsclient.UpdateWithMD{
			MD:  xdsclient.UpdateMetadata{Status: xdsclient.ServiceStatusACKed, Version: testVersion},
			Raw: r,
		}
	}
	updateHandler.NewClusters(update0, xdsclient.UpdateMetadata{Status: xdsclient.ServiceStatusACKed, Version: testVersion})

	// Expect ACK.
	if err := compareDump(client.DumpCDS, testVersion, want0); err != nil {
		t.Fatalf(err.Error())
	}

	const nackVersion = "cds-version-nack"
	var nackErr = fmt.Errorf("cds nack error")
	updateHandler.NewClusters(
		map[string]xdsclient.ClusterUpdateErrTuple{
			cdsTargets[0]: {Err: nackErr},
			cdsTargets[1]: {Update: xdsclient.ClusterUpdate{Raw: clusterRaws[cdsTargets[1]]}},
		},
		xdsclient.UpdateMetadata{
			Status: xdsclient.ServiceStatusNACKed,
			ErrState: &xdsclient.UpdateErrorMetadata{
				Version: nackVersion,
				Err:     nackErr,
			},
		},
	)

	// Expect NACK for [0], but old ACK for [1].
	wantDump := make(map[string]xdsclient.UpdateWithMD)
	// Though resource 0 was NACKed, the dump should show the previous ACKed raw
	// message, as well as the NACK error.
	wantDump[cdsTargets[0]] = xdsclient.UpdateWithMD{
		MD: xdsclient.UpdateMetadata{
			Status:  xdsclient.ServiceStatusNACKed,
			Version: testVersion,
			ErrState: &xdsclient.UpdateErrorMetadata{
				Version: nackVersion,
				Err:     nackErr,
			},
		},
		Raw: clusterRaws[cdsTargets[0]],
	}
	wantDump[cdsTargets[1]] = xdsclient.UpdateWithMD{
		MD:  xdsclient.UpdateMetadata{Status: xdsclient.ServiceStatusACKed, Version: nackVersion},
		Raw: clusterRaws[cdsTargets[1]],
	}
	if err := compareDump(client.DumpCDS, nackVersion, wantDump); err != nil {
		t.Fatalf(err.Error())
	}
}

func (s) TestEDSConfigDump(t *testing.T) {
	const testVersion = "test-version-cds"
	var (
		edsTargets    = []string{"cluster-0", "cluster-1"}
		localityNames = []string{"locality-0", "locality-1"}
		addrs         = []string{"addr0:123", "addr1:456"}
		endpointRaws  = make(map[string]*anypb.Any, len(edsTargets))
	)

	for i := range edsTargets {
		clab0 := xdstestutils.NewClusterLoadAssignmentBuilder(edsTargets[i], nil)
		clab0.AddLocality(localityNames[i], 1, 1, []string{addrs[i]}, nil)
		claT := clab0.Build()

		endpointRaws[edsTargets[i]] = testutils.MarshalAny(claT)
	}

	client, err := xdsclient.NewWithConfigForTesting(&bootstrap.Config{
		XDSServer: &bootstrap.ServerConfig{
			ServerURI: testXDSServer,
			Creds:     grpc.WithTransportCredentials(insecure.NewCredentials()),
			NodeProto: xdstestutils.EmptyNodeProtoV2,
		},
	}, defaultTestWatchExpiryTimeout)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	defer client.Close()
	updateHandler := client.(xdsclient.UpdateHandler)

	// Expected unknown.
	if err := compareDump(client.DumpEDS, "", map[string]xdsclient.UpdateWithMD{}); err != nil {
		t.Fatalf(err.Error())
	}

	wantRequested := make(map[string]xdsclient.UpdateWithMD)
	for _, n := range edsTargets {
		cancel := client.WatchEndpoints(n, func(update xdsclient.EndpointsUpdate, err error) {})
		defer cancel()
		wantRequested[n] = xdsclient.UpdateWithMD{MD: xdsclient.UpdateMetadata{Status: xdsclient.ServiceStatusRequested}}
	}
	// Expected requested.
	if err := compareDump(client.DumpEDS, "", wantRequested); err != nil {
		t.Fatalf(err.Error())
	}

	update0 := make(map[string]xdsclient.EndpointsUpdateErrTuple)
	want0 := make(map[string]xdsclient.UpdateWithMD)
	for n, r := range endpointRaws {
		update0[n] = xdsclient.EndpointsUpdateErrTuple{Update: xdsclient.EndpointsUpdate{Raw: r}}
		want0[n] = xdsclient.UpdateWithMD{
			MD:  xdsclient.UpdateMetadata{Status: xdsclient.ServiceStatusACKed, Version: testVersion},
			Raw: r,
		}
	}
	updateHandler.NewEndpoints(update0, xdsclient.UpdateMetadata{Status: xdsclient.ServiceStatusACKed, Version: testVersion})

	// Expect ACK.
	if err := compareDump(client.DumpEDS, testVersion, want0); err != nil {
		t.Fatalf(err.Error())
	}

	const nackVersion = "eds-version-nack"
	var nackErr = fmt.Errorf("eds nack error")
	updateHandler.NewEndpoints(
		map[string]xdsclient.EndpointsUpdateErrTuple{
			edsTargets[0]: {Err: nackErr},
			edsTargets[1]: {Update: xdsclient.EndpointsUpdate{Raw: endpointRaws[edsTargets[1]]}},
		},
		xdsclient.UpdateMetadata{
			Status: xdsclient.ServiceStatusNACKed,
			ErrState: &xdsclient.UpdateErrorMetadata{
				Version: nackVersion,
				Err:     nackErr,
			},
		},
	)

	// Expect NACK for [0], but old ACK for [1].
	wantDump := make(map[string]xdsclient.UpdateWithMD)
	// Though resource 0 was NACKed, the dump should show the previous ACKed raw
	// message, as well as the NACK error.
	wantDump[edsTargets[0]] = xdsclient.UpdateWithMD{
		MD: xdsclient.UpdateMetadata{
			Status:  xdsclient.ServiceStatusNACKed,
			Version: testVersion,
			ErrState: &xdsclient.UpdateErrorMetadata{
				Version: nackVersion,
				Err:     nackErr,
			},
		},
		Raw: endpointRaws[edsTargets[0]],
	}
	wantDump[edsTargets[1]] = xdsclient.UpdateWithMD{
		MD:  xdsclient.UpdateMetadata{Status: xdsclient.ServiceStatusACKed, Version: nackVersion},
		Raw: endpointRaws[edsTargets[1]],
	}
	if err := compareDump(client.DumpEDS, nackVersion, wantDump); err != nil {
		t.Fatalf(err.Error())
	}
}

func compareDump(dumpFunc func() (string, map[string]xdsclient.UpdateWithMD), wantVersion string, wantDump interface{}) error {
	v, dump := dumpFunc()
	if v != wantVersion {
		return fmt.Errorf("Dump() returned version %q, want %q", v, wantVersion)
	}
	cmpOpts := cmp.Options{
		cmpopts.EquateEmpty(),
		cmp.Comparer(func(a, b time.Time) bool { return true }),
		cmp.Comparer(func(x, y error) bool {
			if x == nil || y == nil {
				return x == nil && y == nil
			}
			return x.Error() == y.Error()
		}),
		protocmp.Transform(),
	}
	if diff := cmp.Diff(dump, wantDump, cmpOpts); diff != "" {
		return fmt.Errorf("Dump() returned unexpected dump, diff (-got +want): %s", diff)
	}
	return nil
}
