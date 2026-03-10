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
	"net"
	"net/http"
	"regexp"
	"strings"

	"k8s.io/klog/v2"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type GatewayState struct {
	*gatewayv1.Gateway
}

func (s *GatewayState) GetHTTPRoutes(allRoutes []*HTTPRouteState, controllerName string) []*HTTPRouteState {
	var matches []*HTTPRouteState
	for _, route := range allRoutes {
		if route.MatchesGateway(s.Gateway, controllerName) {
			matches = append(matches, route)
		}
	}
	return matches
}

func (s *HTTPRouteState) GetHostnames() []string {
	var hostnames []string
	for _, h := range s.Spec.Hostnames {
		hostnames = append(hostnames, string(h))
	}
	return hostnames
}

func (s *HTTPRouteState) GetNamespace() string {
	return s.Namespace
}

// InternalRoute represents the computed state for a route, used by the proxy.
type InternalRoute struct {
	Hostnames []string
	Rules     []InternalRule
}

func (ir *InternalRoute) MatchHostname(host string) bool {
	// Strip port if present
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	if len(ir.Hostnames) == 0 {
		return true
	}
	for _, h := range ir.Hostnames {
		if h == "*" {
			return true
		}
		if h == host {
			return true
		}
		if strings.HasPrefix(h, "*.") {
			suffix := h[1:] // .example.com
			if strings.HasSuffix(host, suffix) {
				return true
			}
		}
	}
	return false
}

// ErrorState represents an error that should be surfaced to the user.
// It includes both the API condition for status reporting and the HTTP response details for the proxy.
type ErrorState struct {
	// Condition is the status condition to be reported in the API.
	// This provides a machine-readable reason for the error.
	Condition metav1.Condition

	// HTTPStatusCode is the status code to return in the HTTP response.
	// This is the "user-facing" status code.
	HTTPStatusCode int

	// HTTPMessage is the message to return in the HTTP response body.
	// This is the "user-facing" error message.
	HTTPMessage string
}

type InternalRule struct {
	Matches  []InternalMatch
	Backend  *InternalBackend
	Redirect *InternalRedirect
	// Error, if non-nil, indicates that this rule is invalid and should
	// return an error response if matched.
	Error *ErrorState
}

type InternalBackend struct {
	Host        string
	Port        int32
	AppProtocol *string
	TLSConfig   *InternalTLSConfig
}

type InternalTLSConfig struct {
	Hostname string
	CACerts  [][]byte
}

type InternalRedirect struct {
	Scheme     *string
	Hostname   *gatewayv1.PreciseHostname
	Path       *InternalPathRedirect
	Port       *gatewayv1.PortNumber
	StatusCode *int
}

type InternalPathRedirect struct {
	Type  gatewayv1.HTTPPathModifierType
	Value string
}

type InternalMatch struct {
	Path        *InternalPathMatch
	Headers     []InternalHeaderMatch
	QueryParams []InternalQueryParamMatch
}

type MatchRequest struct {
	Path   string
	Header http.Header
	Method string

	reqURL *url.URL
	query  map[string][]string
}

func (m *MatchRequest) GetQuery() map[string][]string {
	if m.query == nil {
		if m.reqURL != nil {
			m.query = m.reqURL.Query()
		} else {
			m.query = make(map[string][]string)
		}
	}
	return m.query
}

func (im *InternalMatch) Matches(req *MatchRequest) bool {
	if im.Path != nil {
		switch im.Path.Type {
		case gatewayv1.PathMatchExact:
			if req.Path != im.Path.Value {
				return false
			}
		case gatewayv1.PathMatchPathPrefix:
			if !hasPathPrefix(req.Path, im.Path.Value) {
				return false
			}
		}
	}

	for _, hm := range im.Headers {
		values := req.Header[http.CanonicalHeaderKey(hm.Name)]
		matched := false
		for _, v := range values {
			if hm.Type == gatewayv1.HeaderMatchRegularExpression {
				if hm.MatchRegularExpressionValue != nil && hm.MatchRegularExpressionValue.MatchString(v) {
					matched = true
					break
				}
			} else {
				if v == hm.MatchExactValue {
					matched = true
					break
				}
			}
		}
		if !matched {
			return false
		}
	}

	if len(im.QueryParams) > 0 {
		query := req.GetQuery()
		if len(query) == 0 {
			return false
		}
		for _, qm := range im.QueryParams {
			values := query[qm.Name]
			matched := false
			for _, v := range values {
				if qm.Type == gatewayv1.QueryParamMatchRegularExpression {
					if qm.MatchRegularExpressionValue.MatchString(v) {
						matched = true
						break
					}
				} else if qm.Type == gatewayv1.QueryParamMatchExact {
					if v == qm.MatchExactValue {
						matched = true
						break
					}
				}
			}
			if !matched {
				return false
			}
		}
	}

	return true
}

func hasPathPrefix(path, prefix string) bool {
	if prefix == "/" {
		return true
	}
	if path == prefix {
		return true
	}
	if len(path) > len(prefix) && path[len(prefix)] == '/' && path[:len(prefix)] == prefix {
		return true
	}
	// Also handle case where prefix ends with /
	if len(prefix) > 0 && prefix[len(prefix)-1] == '/' {
		if len(path) >= len(prefix) && path[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

type InternalPathMatch struct {
	Type  gatewayv1.PathMatchType
	Value string
}

type InternalHeaderMatch struct {
	Type                        gatewayv1.HeaderMatchType
	Name                        string
	MatchExactValue             string
	MatchRegularExpressionValue *regexp.Regexp
}

type InternalQueryParamMatch struct {
	Type                        gatewayv1.QueryParamMatchType
	Name                        string
	MatchExactValue             string
	MatchRegularExpressionValue *regexp.Regexp
}

func MatchRoute(routes []InternalRoute, r *http.Request) (*InternalRule, *InternalMatch) {
	var bestRule *InternalRule
	var bestMatch *InternalMatch

	req := &MatchRequest{
		Path:   r.URL.Path,
		Header: r.Header,
		Method: r.Method,
		reqURL: r.URL,
	}

	for i := range routes {
		route := &routes[i]
		if !route.MatchHostname(r.Host) {
			continue
		}

		for j := range route.Rules {
			rule := &route.Rules[j]
			for k := range rule.Matches {
				match := &rule.Matches[k]
				if match.Matches(req) {
					if isBetterMatch(match, bestMatch) {
						bestMatch = match
						bestRule = rule
					}
				}
			}
			if len(rule.Matches) == 0 {
				// Rule with no matches always matches, but is the least specific
				if bestRule == nil {
					bestRule = rule
				}
			}
		}
	}

	return bestRule, bestMatch
}

// MatchWeight represents the precedence of a match.
// Higher values in the fields indicate a better match.
type MatchWeight struct {
	PathTypeWeight  int
	PathLen         int
	HeaderCount     int
	QueryParamCount int
}

func getMatchWeight(m *InternalMatch) MatchWeight {
	if m == nil {
		return MatchWeight{PathTypeWeight: -1}
	}
	return MatchWeight{
		PathTypeWeight:  getPathMatchTypeWeight(getPathMatchType(m)),
		PathLen:         getPathLen(m),
		HeaderCount:     len(m.Headers),
		QueryParamCount: len(m.QueryParams),
	}
}

func isBetterMatch(current, best *InternalMatch) bool {
	if best == nil {
		return true
	}

	cw := getMatchWeight(current)
	bw := getMatchWeight(best)

	if cw.PathTypeWeight != bw.PathTypeWeight {
		return cw.PathTypeWeight > bw.PathTypeWeight
	}
	if cw.PathLen != bw.PathLen {
		return cw.PathLen > bw.PathLen
	}
	if cw.HeaderCount != bw.HeaderCount {
		return cw.HeaderCount > bw.HeaderCount
	}
	return cw.QueryParamCount > bw.QueryParamCount
}

func getPathMatchType(m *InternalMatch) gatewayv1.PathMatchType {
	if m.Path == nil {
		return ""
	}
	return m.Path.Type
}

func getPathMatchTypeWeight(t gatewayv1.PathMatchType) int {
	switch t {
	case gatewayv1.PathMatchExact:
		return 3
	case gatewayv1.PathMatchPathPrefix:
		return 2
	case "":
		return 1
	default:
		return 0
	}
}

func getPathLen(m *InternalMatch) int {
	if m.Path == nil {
		return 0
	}
	return len(m.Path.Value)
}

func (s *GatewayState) BuildInternalRoutes(routes []*HTTPRouteState, services map[types.NamespacedName]*corev1.Service, backendTLSPolicies []*gatewayv1.BackendTLSPolicy, configMaps map[types.NamespacedName]*corev1.ConfigMap, controllerName string) []InternalRoute {
	var internalRoutes []InternalRoute

	for _, listener := range s.Spec.Listeners {
		// Check if listener is compatible with HTTPRoute
		if listener.Protocol != gatewayv1.HTTPProtocolType && listener.Protocol != gatewayv1.HTTPSProtocolType {
			continue
		}

		for _, route := range routes {
			// Check if this route is bound to this Gateway and specifically this listener (if SectionName is set)
			bound := false
			var matchingParentRef *gatewayv1.ParentReference
			for i := range route.Spec.ParentRefs {
				parentRef := &route.Spec.ParentRefs[i]
				if string(parentRef.Name) != s.Name {
					continue
				}
				// Namespace check (optional for now as per current implementation)
				if ns := ValueOf(parentRef.Namespace); ns != "" && string(ns) != s.Namespace {
					continue
				}

				if sn := ValueOf(parentRef.SectionName); sn != "" && sn != listener.Name {
					continue
				}

				// Dynamically compute acceptance for this listener
				if cond := route.ComputeAcceptedCondition(*parentRef, []*GatewayState{s}); cond.Status == metav1.ConditionTrue {
					bound = true
					matchingParentRef = parentRef
					break
				}
			}

			if !bound {
				continue
			}

			// Calculate intersected hostnames
			routeHostnames := route.GetHostnames()
			listenerHostname := ValueOf(listener.Hostname)

			effectiveHostnames := IntersectHostnames(routeHostnames, string(listenerHostname))
			if len(effectiveHostnames) == 0 && len(routeHostnames) > 0 {
				// No intersection, skip this listener
				continue
			}

			ir := InternalRoute{
				Hostnames: effectiveHostnames,
			}

			resolvedRefsCond := route.ComputeResolvedRefsCondition()

			for _, rule := range route.Spec.Rules {
				var redirect *InternalRedirect
				for _, filter := range rule.Filters {
					if filter.Type == gatewayv1.HTTPRouteFilterRequestRedirect {
						r := filter.RequestRedirect
						redirect = &InternalRedirect{
							Scheme:     r.Scheme,
							Hostname:   r.Hostname,
							Port:       r.Port,
							StatusCode: r.StatusCode,
						}
						if r.Path != nil {
							var pathValue string
							if r.Path.Type == gatewayv1.FullPathHTTPPathModifier {
								pathValue = ValueOf(r.Path.ReplaceFullPath)
							} else if r.Path.Type == gatewayv1.PrefixMatchHTTPPathModifier {
								pathValue = ValueOf(r.Path.ReplacePrefixMatch)
							}
							redirect.Path = &InternalPathRedirect{
								Type:  r.Path.Type,
								Value: pathValue,
							}
						}
						break
					}
				}

				iRule := InternalRule{
					Redirect: redirect,
				}

				if resolvedRefsCond.Status == metav1.ConditionFalse {
					iRule.Error = &ErrorState{
						Condition:      resolvedRefsCond,
						HTTPStatusCode: http.StatusInternalServerError,
						HTTPMessage:    resolvedRefsCond.Message,
					}
				} else if redirect == nil {
					for _, backendRef := range rule.BackendRefs {
						kind := ValueOf(backendRef.Kind)
						if kind == "" {
							kind = "Service"
						}
						if kind != "Service" {
							iRule.Error = &ErrorState{
								Condition: metav1.Condition{
									Type:    string(gatewayv1.RouteConditionResolvedRefs),
									Status:  metav1.ConditionFalse,
									Reason:  string(gatewayv1.RouteReasonInvalidKind),
									Message: fmt.Sprintf("Unsupported backend kind: %s", kind),
								},
								HTTPStatusCode: http.StatusInternalServerError,
								HTTPMessage:    fmt.Sprintf("Unsupported backend kind: %s", kind),
							}
							continue
						}

						if backendRef.Port == nil {
							continue
						}

						backendSvcNamespace := route.Namespace
						if backendRef.Namespace != nil {
							backendSvcNamespace = string(*backendRef.Namespace)
						}

						backendSvcName := types.NamespacedName{
							Namespace: backendSvcNamespace,
							Name:      string(backendRef.Name),
						}
						var appProtocol *string
						if svc, ok := services[backendSvcName]; ok {
							for _, port := range svc.Spec.Ports {
								if port.Port == int32(*backendRef.Port) {
									appProtocol = port.AppProtocol
									break
								}
							}
						}

						// Check for BackendTLSPolicy
						var tlsConfig *InternalTLSConfig
						for _, policy := range backendTLSPolicies {
							for _, targetRef := range policy.Spec.TargetRefs {
								if string(targetRef.Group) == "" && string(targetRef.Kind) == "Service" &&
									string(targetRef.Name) == string(backendRef.Name) &&
									policy.Namespace == backendSvcNamespace {
									// Found a policy targeting this service
									https := "https"
									appProtocol = &https

									var caCerts [][]byte
									for _, caRef := range policy.Spec.Validation.CACertificateRefs {
										if string(caRef.Group) == "" && string(caRef.Kind) == "ConfigMap" {
											cmName := types.NamespacedName{Namespace: policy.Namespace, Name: string(caRef.Name)}
											if cm, ok := configMaps[cmName]; ok {
												if data, ok := cm.Data["ca.crt"]; ok {
													caCerts = append(caCerts, []byte(data))
												} else if data, ok := cm.BinaryData["ca.crt"]; ok {
													caCerts = append(caCerts, data)
												}
											}
										}
									}

									tlsConfig = &InternalTLSConfig{
										Hostname: string(policy.Spec.Validation.Hostname),
										CACerts:  caCerts,
									}
									break
								}
							}
						}

						iRule.Backend = &InternalBackend{
							Host:        fmt.Sprintf("%s.%s.svc.cluster.local", backendRef.Name, backendSvcNamespace),
							Port:        int32(*backendRef.Port),
							AppProtocol: appProtocol,
							TLSConfig:   tlsConfig,
						}
						iRule.Error = nil

						// For minimal implementation, we just take the first Service backendRef for each rule
						break
					}
				}

				for _, match := range rule.Matches {
					iMatch := InternalMatch{}
					if match.Path != nil {
						iMatch.Path = &InternalPathMatch{
							Type:  ValueOrDefault(match.Path.Type, gatewayv1.PathMatchPathPrefix),
							Value: ValueOf(match.Path.Value),
						}
					}
					for _, header := range match.Headers {
						headerType := ValueOrDefault(header.Type, gatewayv1.HeaderMatchExact)
						hm := InternalHeaderMatch{
							Type:            headerType,
							Name:            string(header.Name),
							MatchExactValue: header.Value,
						}
						if headerType == gatewayv1.HeaderMatchRegularExpression {
							re, err := regexp.Compile(header.Value)
							if err == nil {
								hm.MatchRegularExpressionValue = re
							}
						}
						iMatch.Headers = append(iMatch.Headers, hm)
					}
					seenQueryParams := make(map[string]bool)
					for _, queryParam := range match.QueryParams {
						name := string(queryParam.Name)
						if seenQueryParams[name] {
							continue
						}
						seenQueryParams[name] = true

						queryParamType := ValueOrDefault(queryParam.Type, gatewayv1.QueryParamMatchExact)

						if queryParamType != gatewayv1.QueryParamMatchExact && queryParamType != gatewayv1.QueryParamMatchRegularExpression {
							klog.Errorf("Unsupported query parameter match type: %s", queryParamType)
							continue
						}

						qm := InternalQueryParamMatch{
							Type:            queryParamType,
							Name:            name,
							MatchExactValue: queryParam.Value,
						}

						if queryParamType == gatewayv1.QueryParamMatchRegularExpression {
							re, err := regexp.Compile(queryParam.Value)
							if err != nil {
								klog.Errorf("Invalid regular expression for query parameter match %q: %v", name, err)
								continue // skip appending this invalid match
							}
							qm.MatchRegularExpressionValue = re
						}
						iMatch.QueryParams = append(iMatch.QueryParams, qm)
					}
					iRule.Matches = append(iRule.Matches, iMatch)
				}

				ir.Rules = append(ir.Rules, iRule)
			}
			internalRoutes = append(internalRoutes, ir)
			_ = matchingParentRef // keep for now
		}
	}

	return internalRoutes
}
