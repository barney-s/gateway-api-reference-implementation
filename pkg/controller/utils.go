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

package controller

import (
	"context"
	"reflect"

	"github.com/gke-labs/gateway-api-reference-implementation/pkg/proxy"
	"github.com/gke-labs/gateway-api-reference-implementation/pkg/state"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func updateProxy(st *state.State, p *proxy.Proxy) {
	gateways := st.GetGateways()

	ctx := state.BuildInternalRoutesContext{
		HTTPRoutes:         st.GetHTTPRoutes(),
		GRPCRoutes:         st.GetGRPCRoutes(),
		Services:           st.GetServices(),
		BackendTLSPolicies: st.GetBackendTLSPolicies(),
		ConfigMaps:         st.GetConfigMaps(),
		ControllerName:     ControllerName,
	}

	var proxyRoutes []state.InternalRoute
	for _, gw := range gateways {
		proxyRoutes = append(proxyRoutes, gw.BuildInternalRoutes(ctx)...)
	}
	p.UpdateRoutes(proxyRoutes)
}

func mergeParents(existing []gatewayv1.RouteParentStatus, newParents []gatewayv1.RouteParentStatus, controllerName gatewayv1.GatewayController) ([]gatewayv1.RouteParentStatus, bool) {
	var merged []gatewayv1.RouteParentStatus
	updated := false

	var existingForUs []gatewayv1.RouteParentStatus
	for _, p := range existing {
		if string(p.ControllerName) == string(controllerName) {
			existingForUs = append(existingForUs, p)
		} else {
			merged = append(merged, p)
		}
	}

	merged = append(merged, newParents...)

	if len(existingForUs) != len(newParents) {
		updated = true
	} else {
		for i := range newParents {
			if !reflect.DeepEqual(existingForUs[i].ParentRef, newParents[i].ParentRef) ||
				len(existingForUs[i].Conditions) != len(newParents[i].Conditions) {
				updated = true
				break
			}
			for j := range newParents[i].Conditions {
				matched := false
				for k := range existingForUs[i].Conditions {
					if existingForUs[i].Conditions[k].Type == newParents[i].Conditions[j].Type {
						if existingForUs[i].Conditions[k].Status == newParents[i].Conditions[j].Status &&
							existingForUs[i].Conditions[k].ObservedGeneration == newParents[i].Conditions[j].ObservedGeneration &&
							existingForUs[i].Conditions[k].Reason == newParents[i].Conditions[j].Reason &&
							existingForUs[i].Conditions[k].Message == newParents[i].Conditions[j].Message {
							matched = true
						}
						break
					}
				}
				if !matched {
					updated = true
					break
				}
			}
			if updated {
				break
			}
		}
	}

	return merged, updated
}

type gatewayEventHandler struct {
	State  *state.State
	Client client.Client
	Kind   string
}

func (h *gatewayEventHandler) Create(ctx context.Context, e event.TypedCreateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.enqueueRoutes(ctx, q)
}

func (h *gatewayEventHandler) Update(ctx context.Context, e event.TypedUpdateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.enqueueRoutes(ctx, q)
}

func (h *gatewayEventHandler) Delete(ctx context.Context, e event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.enqueueRoutes(ctx, q)
}

func (h *gatewayEventHandler) Generic(ctx context.Context, e event.TypedGenericEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.enqueueRoutes(ctx, q)
}

func (h *gatewayEventHandler) enqueueRoutes(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if h.Kind == "GRPCRoute" {
		for _, route := range h.State.GetGRPCRoutes() {
			q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
				Name:      route.Name,
				Namespace: route.Namespace,
			}})
		}
	} else if h.Kind == "HTTPRoute" {
		for _, route := range h.State.GetHTTPRoutes() {
			q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
				Name:      route.Name,
				Namespace: route.Namespace,
			}})
		}
	}
}
