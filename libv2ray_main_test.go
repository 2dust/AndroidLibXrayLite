package libv2ray

import (
	"reflect"
	"testing"

	coreobservatory "github.com/xtls/xray-core/app/observatory"
	corerouter "github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common/serial"
	core "github.com/xtls/xray-core/core"
)

func TestRoutedBalancerSelectors(t *testing.T) {
	routerConfig := &corerouter.Config{
		Rule: []*corerouter.RoutingRule{{
			TargetTag: &corerouter.RoutingRule_BalancingTag{BalancingTag: "balancer-main"},
		}},
		BalancingRule: []*corerouter.BalancingRule{{
			Tag:              "balancer-main",
			OutboundSelector: []string{"proxy-policy-"},
		}},
	}
	config := &core.Config{App: []*serial.TypedMessage{
		serial.ToTypedMessage(routerConfig),
		serial.ToTypedMessage(&coreobservatory.Config{SubjectSelector: []string{"proxy-policy-"}}),
	}}

	selectors, err := routedBalancerSelectors(config)
	if err != nil {
		t.Fatalf("routedBalancerSelectors returned an error: %v", err)
	}
	if want := []string{"proxy-policy-"}; !reflect.DeepEqual(selectors, want) {
		t.Fatalf("selectors = %v, want %v", selectors, want)
	}
}

func TestRoutedBalancerSelectorsWithoutObservatory(t *testing.T) {
	routerConfig := &corerouter.Config{
		Rule: []*corerouter.RoutingRule{{
			TargetTag: &corerouter.RoutingRule_BalancingTag{BalancingTag: "balancer-main"},
		}},
	}
	config := &core.Config{App: []*serial.TypedMessage{serial.ToTypedMessage(routerConfig)}}

	selectors, err := routedBalancerSelectors(config)
	if err != nil {
		t.Fatalf("routedBalancerSelectors returned an error: %v", err)
	}
	if selectors != nil {
		t.Fatalf("selectors = %v, want nil", selectors)
	}
}

func TestPolicyGroupObservationState(t *testing.T) {
	candidates := map[string]struct{}{"a": {}, "b": {}}
	tests := []struct {
		name     string
		statuses []*coreobservatory.OutboundStatus
		ready    bool
		complete bool
	}{
		{name: "empty"},
		{
			name:     "partial dead",
			statuses: []*coreobservatory.OutboundStatus{{OutboundTag: "a"}},
		},
		{
			name:     "first healthy",
			statuses: []*coreobservatory.OutboundStatus{{OutboundTag: "a", Alive: true}},
			ready:    true,
		},
		{
			name: "all dead",
			statuses: []*coreobservatory.OutboundStatus{
				{OutboundTag: "a"},
				{OutboundTag: "b"},
			},
			complete: true,
		},
		{
			name: "ignores unrelated status",
			statuses: []*coreobservatory.OutboundStatus{
				{OutboundTag: "unrelated", Alive: true},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ready, complete := policyGroupObservationState(
				&coreobservatory.ObservationResult{Status: test.statuses},
				candidates,
			)
			if ready != test.ready || complete != test.complete {
				t.Fatalf("state = (%v, %v), want (%v, %v)", ready, complete, test.ready, test.complete)
			}
		})
	}
}
