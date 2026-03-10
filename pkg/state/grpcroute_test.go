// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package state

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestGRPCRouteComputeAcceptedCondition(t *testing.T) {
	tests := []struct {
		name           string
		routeHostnames []gatewayv1.Hostname
		listenerHost   *gatewayv1.Hostname
		wantAccepted   bool
		wantReason     gatewayv1.RouteConditionReason
	}{
		{
			name:           "matching hostname",
			routeHostnames: []gatewayv1.Hostname{"foo.com"},
			listenerHost:   Ptr(gatewayv1.Hostname("foo.com")),
			wantAccepted:   true,
			wantReason:     gatewayv1.RouteReasonAccepted,
		},
		{
			name:           "mismatching hostname",
			routeHostnames: []gatewayv1.Hostname{"foo.com"},
			listenerHost:   Ptr(gatewayv1.Hostname("bar.com")),
			wantAccepted:   false,
			wantReason:     gatewayv1.RouteReasonNoMatchingListenerHostname,
		},
		{
			name:           "wildcard listener matches route",
			routeHostnames: []gatewayv1.Hostname{"foo.com"},
			listenerHost:   Ptr(gatewayv1.Hostname("*.com")),
			wantAccepted:   true,
			wantReason:     gatewayv1.RouteReasonAccepted,
		},
		{
			name:           "no route hostnames matches any listener",
			routeHostnames: nil,
			listenerHost:   Ptr(gatewayv1.Hostname("foo.com")),
			wantAccepted:   true,
			wantReason:     gatewayv1.RouteReasonAccepted,
		},
		{
			name:           "empty listener hostname matches everything",
			routeHostnames: []gatewayv1.Hostname{"foo.com"},
			listenerHost:   nil,
			wantAccepted:   true,
			wantReason:     gatewayv1.RouteReasonAccepted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route := &gatewayv1.GRPCRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-route",
				},
				Spec: gatewayv1.GRPCRouteSpec{
					Hostnames: tt.routeHostnames,
				},
			}
			var hostnames []string
			for _, h := range route.Spec.Hostnames {
				hostnames = append(hostnames, string(h))
			}
			s := &GRPCRouteState{GRPCRoute: route, Hostnames: hostnames}
			gw := &GatewayState{
				Gateway: &gatewayv1.Gateway{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-gw",
					},
					Spec: gatewayv1.GatewaySpec{
						Listeners: []gatewayv1.Listener{
							{
								Name:     "http",
								Protocol: gatewayv1.HTTPProtocolType,
								Hostname: tt.listenerHost,
							},
						},
					},
				},
			}

			parentRef := gatewayv1.ParentReference{
				Name: "test-gw",
			}

			cond := s.ComputeAcceptedCondition(parentRef, []*GatewayState{gw})

			if tt.wantAccepted {
				if cond.Status != metav1.ConditionTrue {
					t.Errorf("ComputeAcceptedCondition() status = %v, want %v", cond.Status, metav1.ConditionTrue)
				}
			} else {
				if cond.Status != metav1.ConditionFalse {
					t.Errorf("ComputeAcceptedCondition() status = %v, want %v", cond.Status, metav1.ConditionFalse)
				}
			}

			if string(cond.Reason) != string(tt.wantReason) {
				t.Errorf("ComputeAcceptedCondition() reason = %v, want %v", cond.Reason, tt.wantReason)
			}
		})
	}
}

func TestGRPCRouteValidate(t *testing.T) {
	route := &gatewayv1.GRPCRoute{
		Spec: gatewayv1.GRPCRouteSpec{
			Rules: []gatewayv1.GRPCRouteRule{
				{
					Matches: []gatewayv1.GRPCRouteMatch{
						{
							Method: &gatewayv1.GRPCMethodMatch{
								Type:    Ptr(gatewayv1.GRPCMethodMatchRegularExpression),
								Service: Ptr("invalid[regex"),
							},
						},
					},
				},
			},
		},
	}
	rs := &GRPCRouteState{GRPCRoute: route}
	err := rs.Validate()
	if err == nil {
		t.Errorf("Expected validation error for invalid regex")
	}
}

func TestGRPCRouteComputeResolvedRefsCondition(t *testing.T) {
	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec: gatewayv1.GRPCRouteSpec{
			Rules: []gatewayv1.GRPCRouteRule{
				{
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "nonexistent",
									Kind: Ptr(gatewayv1.Kind("Service")),
								},
							},
						},
					},
				},
			},
		},
	}
	rs := &GRPCRouteState{GRPCRoute: route}
	services := map[types.NamespacedName]*corev1.Service{}

	cond := rs.ComputeResolvedRefsCondition(services)
	if cond.Status != metav1.ConditionFalse || cond.Reason != string(gatewayv1.RouteReasonBackendNotFound) {
		t.Errorf("Expected RouteReasonBackendNotFound, got %v", cond.Reason)
	}
}
