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

	"github.com/gke-labs/gateway-api-reference-implementation/pkg/proxy"
	"github.com/gke-labs/gateway-api-reference-implementation/pkg/state"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"errors"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type GRPCRouteReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	State  *state.State
	Proxy  *proxy.Proxy
}

func (r *GRPCRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithValues("grpcroute", req.NamespacedName)

	route := &gatewayv1.GRPCRoute{}
	if err := r.Get(ctx, req.NamespacedName, route); err != nil {
		if apierrors.IsNotFound(err) {
			r.State.DeleteGRPCRoute(req.NamespacedName)
			r.updateProxy()
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !route.DeletionTimestamp.IsZero() {
		// Route is being deleted, ignore or handle finalizer
		return ctrl.Result{}, nil
	}

	gateways := r.State.GetGateways()
	
	// Create a temporary state just to validate and compute conditions
	var hostnames []string
	for _, h := range route.Spec.Hostnames {
		hostnames = append(hostnames, string(h))
	}
	rs := &state.GRPCRouteState{GRPCRoute: route, Hostnames: hostnames}
	
	validationCondition := metav1.Condition{
		Type:               string(gatewayv1.RouteConditionAccepted),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: route.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             string(gatewayv1.RouteReasonAccepted),
		Message:            "Route accepted by reference implementation",
	}
	
	if err := rs.Validate(); err != nil {
		validationCondition.Status = metav1.ConditionFalse
		var valErr *state.ValidationError
		if errors.As(err, &valErr) {
			validationCondition.Reason = string(valErr.Reason)
		} else {
			validationCondition.Reason = string(gatewayv1.RouteReasonUnsupportedValue)
		}
		validationCondition.Message = err.Error()
	}

	var newParents []gatewayv1.RouteParentStatus
	for _, parentRef := range route.Spec.ParentRefs {
		acceptedCondition := validationCondition
		if acceptedCondition.Status == metav1.ConditionTrue {
			acceptedCondition = rs.ComputeAcceptedCondition(parentRef, gateways)
		}

		newParents = append(newParents, gatewayv1.RouteParentStatus{
			ParentRef:      parentRef,
			ControllerName: ControllerName,
			Conditions: []metav1.Condition{
				acceptedCondition,
				rs.ComputeResolvedRefsCondition(r.State.GetServices()),
			},
		})
	}

	// Merge with existing parents to not overwrite other controllers' statuses
	var mergedParents []gatewayv1.RouteParentStatus
	for _, existingParent := range route.Status.Parents {
		if existingParent.ControllerName != ControllerName {
			mergedParents = append(mergedParents, existingParent)
		}
	}
	
	// Preserve LastTransitionTime if condition status/reason didn't change
	for i, np := range newParents {
		for j, nc := range np.Conditions {
			// Find existing condition
			var existingCond *metav1.Condition
			for _, ep := range route.Status.Parents {
				if ep.ControllerName == ControllerName && string(ep.ParentRef.Name) == string(np.ParentRef.Name) {
					if ep.ParentRef.Namespace == nil && np.ParentRef.Namespace == nil || (ep.ParentRef.Namespace != nil && np.ParentRef.Namespace != nil && *ep.ParentRef.Namespace == *np.ParentRef.Namespace) {
						if ep.ParentRef.SectionName == nil && np.ParentRef.SectionName == nil || (ep.ParentRef.SectionName != nil && np.ParentRef.SectionName != nil && *ep.ParentRef.SectionName == *np.ParentRef.SectionName) {
							for _, ec := range ep.Conditions {
								if ec.Type == nc.Type {
									existingCond = &ec
									break
								}
							}
						}
					}
				}
			}
			if existingCond != nil && existingCond.Status == nc.Status && existingCond.Reason == nc.Reason {
				newParents[i].Conditions[j].LastTransitionTime = existingCond.LastTransitionTime
			}
		}
		mergedParents = append(mergedParents, newParents[i])
	}

	updated := false
	if len(route.Status.Parents) != len(mergedParents) {
		updated = true
	} else {
		// simplistic check: if anything changed
		for i := range mergedParents {
			matchedParent := false
			for j := range route.Status.Parents {
				if route.Status.Parents[j].ControllerName == mergedParents[i].ControllerName && 
				   string(route.Status.Parents[j].ParentRef.Name) == string(mergedParents[i].ParentRef.Name) {
				   
				    // match namespace and section name
				    nsMatch := (route.Status.Parents[j].ParentRef.Namespace == nil && mergedParents[i].ParentRef.Namespace == nil) || 
				               (route.Status.Parents[j].ParentRef.Namespace != nil && mergedParents[i].ParentRef.Namespace != nil && *route.Status.Parents[j].ParentRef.Namespace == *mergedParents[i].ParentRef.Namespace)
				    secMatch := (route.Status.Parents[j].ParentRef.SectionName == nil && mergedParents[i].ParentRef.SectionName == nil) ||
				                (route.Status.Parents[j].ParentRef.SectionName != nil && mergedParents[i].ParentRef.SectionName != nil && *route.Status.Parents[j].ParentRef.SectionName == *mergedParents[i].ParentRef.SectionName)
				                
				    if nsMatch && secMatch {
						if len(route.Status.Parents[j].Conditions) == len(mergedParents[i].Conditions) {
							allCondsMatched := true
							for k := range mergedParents[i].Conditions {
								condMatched := false
								for l := range route.Status.Parents[j].Conditions {
									if route.Status.Parents[j].Conditions[l].Type == mergedParents[i].Conditions[k].Type &&
										route.Status.Parents[j].Conditions[l].Status == mergedParents[i].Conditions[k].Status &&
										route.Status.Parents[j].Conditions[l].Reason == mergedParents[i].Conditions[k].Reason &&
										route.Status.Parents[j].Conditions[l].Message == mergedParents[i].Conditions[k].Message &&
										route.Status.Parents[j].Conditions[l].ObservedGeneration == mergedParents[i].Conditions[k].ObservedGeneration {
										condMatched = true
										break
									}
								}
								if !condMatched {
									allCondsMatched = false
									break
								}
							}
							if allCondsMatched {
								matchedParent = true
							}
						}
					}
				}
			}
			if !matchedParent {
				updated = true
				break
			}
		}
	}

	if updated {
		route.Status.Parents = mergedParents
		if err := r.Status().Update(ctx, route); err != nil {
			l.Error(err, "unable to update GRPCRoute status")
			return ctrl.Result{}, err
		}
	}

	r.State.UpsertGRPCRoute(route)
	r.updateProxy()

	if updated {
		l.Info("Updated GRPCRoute status and proxy")
	}

	return ctrl.Result{}, nil
}

func (r *GRPCRouteReconciler) updateProxy() {
	updateProxy(r.State, r.Proxy)
}


func (r *GRPCRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.GRPCRoute{}).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				// Requeue all GRPCRoutes
				routes := r.State.GetGRPCRoutes()
				var reqs []reconcile.Request
				for _, route := range routes {
					reqs = append(reqs, reconcile.Request{
						NamespacedName: client.ObjectKey{Namespace: route.Namespace, Name: route.Name},
					})
				}
				return reqs
			}),
		).
		Complete(r)
}
