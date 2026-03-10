package state

import gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

type ValidationError struct {
	Reason  gatewayv1.RouteConditionReason
	Message string
}

func (e *ValidationError) Error() string {
	return e.Message
}
