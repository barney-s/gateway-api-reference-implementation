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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"regexp"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type GRPCRouteState struct {
	*gatewayv1.GRPCRoute
	Hostnames []string
}

func (s *GRPCRouteState) Validate() error {
	if s.GRPCRoute == nil {
		return nil
	}
	for _, rule := range s.Spec.Rules {
		for _, match := range rule.Matches {
			if match.Method != nil && match.Method.Type != nil && *match.Method.Type == gatewayv1.GRPCMethodMatchRegularExpression {
				if match.Method.Service != nil {
					if _, err := regexp.Compile(*match.Method.Service); err != nil {
						return &ValidationError{Reason: gatewayv1.RouteReasonUnsupportedValue, Message: fmt.Sprintf("invalid regular expression in method service match: %v", err)}
					}
				}
				if match.Method.Method != nil {
					if _, err := regexp.Compile(*match.Method.Method); err != nil {
						return &ValidationError{Reason: gatewayv1.RouteReasonUnsupportedValue, Message: fmt.Sprintf("invalid regular expression in method method match: %v", err)}
					}
				}
			}
			for _, header := range match.Headers {
				if header.Type != nil && *header.Type == gatewayv1.GRPCHeaderMatchRegularExpression {
					if _, err := regexp.Compile(header.Value); err != nil {
						return &ValidationError{Reason: gatewayv1.RouteReasonUnsupportedValue, Message: fmt.Sprintf("invalid regular expression in header match: %v", err)}
					}
				}
			}
		}
	}
	return nil
}

func (s *GRPCRouteState) ComputeAcceptedCondition(parentRef gatewayv1.ParentReference, gateways []*GatewayState) metav1.Condition {
	acceptedStatus := metav1.ConditionTrue
	acceptedReason := gatewayv1.RouteReasonAccepted
	acceptedMessage := "Route accepted by reference implementation"

	// Validation is assumed to be checked before calling this or computed based on parent condition
	var gw *GatewayState
	for _, g := range gateways {
		if g.Name == string(parentRef.Name) {
			if parentRef.Namespace != nil && string(*parentRef.Namespace) != g.Namespace {
				continue
			} else if parentRef.Namespace == nil && s.Namespace != g.Namespace {
				continue
			}
			gw = g
			break
		}
	}

	if gw == nil {
		acceptedStatus = metav1.ConditionFalse
		acceptedReason = gatewayv1.RouteReasonNoMatchingParent
		acceptedMessage = "Gateway not found"
	} else {
		matched := false
		for _, listener := range gw.Spec.Listeners {
			if listener.Protocol != gatewayv1.HTTPProtocolType && listener.Protocol != gatewayv1.HTTPSProtocolType {
				continue
			}

			// Namespace allowed route check
			routeNs := s.Namespace
			allowedNs := listener.AllowedRoutes
			if allowedNs != nil && allowedNs.Namespaces != nil && allowedNs.Namespaces.From != nil {
				from := *allowedNs.Namespaces.From
				if from == gatewayv1.NamespacesFromSame && routeNs != gw.Namespace {
					continue
				}
				// We don't implement full Selector logic here for simplicity, but we check if it's strictly Same
			}

			if sectionName := ValueOf(parentRef.SectionName); sectionName != "" && sectionName != listener.Name {
				continue
			}
			if parentRef.Port != nil && *parentRef.Port != listener.Port {
				continue
			}

			effectiveHostnames := IntersectHostnames(s.Hostnames, string(ValueOf(listener.Hostname)))
			if len(effectiveHostnames) > 0 || len(s.Spec.Hostnames) == 0 {
				matched = true
				break
			}
		}
		if !matched {
			acceptedStatus = metav1.ConditionFalse
			if parentRef.SectionName != nil {
				// Check if the section name even exists
				sectionExists := false
				for _, listener := range gw.Spec.Listeners {
					if listener.Name == *parentRef.SectionName {
						sectionExists = true
						break
					}
				}
				if !sectionExists {
					acceptedReason = gatewayv1.RouteReasonNoMatchingParent
				} else {
					acceptedReason = gatewayv1.RouteReasonNoMatchingListenerHostname
				}
			} else {
				acceptedReason = gatewayv1.RouteReasonNoMatchingListenerHostname
			}
			acceptedMessage = "No matching listener hostname or allowed routes"
		}
	}

	return metav1.Condition{
		Type:               string(gatewayv1.RouteConditionAccepted),
		Status:             acceptedStatus,
		ObservedGeneration: s.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             string(acceptedReason),
		Message:            acceptedMessage,
	}
}

func (s *GRPCRouteState) ComputeResolvedRefsCondition(services map[types.NamespacedName]*corev1.Service) metav1.Condition {
	resolvedRefsStatus := metav1.ConditionTrue
	resolvedRefsReason := gatewayv1.RouteReasonResolvedRefs
	resolvedRefsMessage := "All references resolved"

	for _, rule := range s.Spec.Rules {
		for _, backendRef := range rule.BackendRefs {
			group := ValueOf(backendRef.Group)
			if group != "" && group != "core" {
				resolvedRefsStatus = metav1.ConditionFalse
				resolvedRefsReason = gatewayv1.RouteReasonInvalidKind
				resolvedRefsMessage = fmt.Sprintf("Unsupported backend group: %s", group)
				goto done
			}

			kind := ValueOf(backendRef.Kind)
			if kind != "" && kind != "Service" {
				resolvedRefsStatus = metav1.ConditionFalse
				resolvedRefsReason = gatewayv1.RouteReasonInvalidKind
				resolvedRefsMessage = fmt.Sprintf("Unsupported backend kind: %s", kind)
				goto done
			}

			ns := s.Namespace
			if backendRef.Namespace != nil {
				ns = string(*backendRef.Namespace)
			}

			// Note: In a real implementation we would also check ReferenceGrants for cross-namespace
			if ns != s.Namespace {
				// Not perfectly checking ReferenceGrants in this stub, but failing by default as required
				resolvedRefsStatus = metav1.ConditionFalse
				resolvedRefsReason = gatewayv1.RouteReasonRefNotPermitted
				resolvedRefsMessage = "Cross-namespace references require ReferenceGrant (not implemented)"
				goto done
			}

			// Check if service exists
			found := false
			if _, ok := services[types.NamespacedName{Namespace: ns, Name: string(backendRef.Name)}]; ok {
				found = true
			}
			if !found {
				resolvedRefsStatus = metav1.ConditionFalse
				resolvedRefsReason = gatewayv1.RouteReasonBackendNotFound
				resolvedRefsMessage = fmt.Sprintf("Backend service not found: %s", backendRef.Name)
				goto done
			}
		}
	}

done:
	return metav1.Condition{
		Type:               string(gatewayv1.RouteConditionResolvedRefs),
		Status:             resolvedRefsStatus,
		ObservedGeneration: s.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             string(resolvedRefsReason),
		Message:            resolvedRefsMessage,
	}
}

func (s *GRPCRouteState) IsAccepted(controllerName string) bool {
	if s.GRPCRoute == nil {
		return false
	}
	for _, ps := range s.GRPCRoute.Status.Parents {
		if string(ps.ControllerName) == controllerName {
			for _, c := range ps.Conditions {
				if c.Type == string(gatewayv1.RouteConditionAccepted) && c.Status == metav1.ConditionTrue && c.ObservedGeneration == s.Generation {
					return true
				}
			}
		}
	}
	return false
}

func (s *GRPCRouteState) MatchesGateway(gw *gatewayv1.Gateway, controllerName string) bool {
	if s.GRPCRoute == nil {
		return false
	}

	for _, ps := range s.GRPCRoute.Status.Parents {
		if string(ps.ControllerName) == controllerName {
			if string(ps.ParentRef.Name) == gw.Name {
				if ps.ParentRef.Namespace != nil && string(*ps.ParentRef.Namespace) != gw.Namespace {
					continue
				} else if ps.ParentRef.Namespace == nil && s.Namespace != gw.Namespace {
					continue
				}
				for _, c := range ps.Conditions {
					if c.Type == string(gatewayv1.RouteConditionAccepted) && c.Status == metav1.ConditionTrue && c.ObservedGeneration == s.Generation {
						return true
					}
				}
			}
		}
	}

	return false
}
