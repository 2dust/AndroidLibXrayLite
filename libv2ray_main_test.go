package libv2ray

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	coreobservatory "github.com/xtls/xray-core/app/observatory"
	corerouter "github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common/serial"
	core "github.com/xtls/xray-core/core"
)

type resetTestCallback struct {
	statuses []string
}

func (*resetTestCallback) Startup() int { return 0 }
func (*resetTestCallback) Shutdown() int { return 0 }
func (c *resetTestCallback) OnEmitStatus(_ int, status string) int {
	c.statuses = append(c.statuses, status)
	return 0
}
func (*resetTestCallback) OnBalancerTargetChanged(string, string) int { return 0 }

func TestResetNetworkStateRecoversOriginalConfiguration(t *testing.T) {
	callback := &resetTestCallback{}
	controller := &CoreController{
		CallbackHandler: callback,
		configContent:   "config",
		IsRunning:       true,
	}

	type attempt struct {
		config      string
		balancerTag string
		target      string
	}
	var attempts []attempt
	err := controller.resetNetworkStateWithStarter("balancer", "cached-target", func(config, balancerTag, target string) error {
		attempts = append(attempts, attempt{config, balancerTag, target})
		if len(attempts) == 1 {
			return errors.New("replacement failed")
		}
		controller.IsRunning = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !controller.IsRunning {
		t.Fatal("controller was not running after original configuration recovery")
	}
	wantAttempts := []attempt{
		{config: "config", balancerTag: "balancer", target: "cached-target"},
		{config: "config"},
	}
	if !reflect.DeepEqual(attempts, wantAttempts) {
		t.Fatalf("attempts = %#v, want %#v", attempts, wantAttempts)
	}
	if len(callback.statuses) != 1 || !strings.Contains(callback.statuses[0], "recovered") {
		t.Fatalf("statuses = %q, want one recovery status", callback.statuses)
	}
}

func TestResetNetworkStateReportsFailedRecovery(t *testing.T) {
	callback := &resetTestCallback{}
	controller := &CoreController{
		CallbackHandler: callback,
		configContent:   "config",
		IsRunning:       true,
	}

	attempts := 0
	err := controller.resetNetworkStateWithStarter("balancer", "cached-target", func(string, string, string) error {
		attempts++
		return errors.New("startup failed")
	})
	if err == nil {
		t.Fatal("expected reset and recovery failure")
	}
	if attempts != 2 {
		t.Fatalf("startup attempts = %d, want 2", attempts)
	}
	if controller.IsRunning {
		t.Fatal("controller unexpectedly remained running after both startup attempts failed")
	}
	if !strings.Contains(err.Error(), "original configuration recovery failed") {
		t.Fatalf("error = %q, want recovery failure detail", err)
	}
	if len(callback.statuses) != 1 || !strings.Contains(callback.statuses[0], "recovery failed") {
		t.Fatalf("statuses = %q, want one failed recovery status", callback.statuses)
	}
}

func TestRoutedBalancerSelectors(t *testing.T) {
	routerConfig := &corerouter.Config{
		Rule: []*corerouter.RoutingRule{{
			TargetTag: &corerouter.RoutingRule_BalancingTag{BalancingTag: "balancer-main"},
		}},
		BalancingRule: []*corerouter.BalancingRule{{
			Tag:              "balancer-main",
			OutboundSelector: []string{"proxy-policy-"},
			Strategy:         "leastPing",
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
	plan, err := routedBalancerPlanForConfig(config)
	if err != nil {
		t.Fatalf("routedBalancerPlanForConfig returned an error: %v", err)
	}
	if plan.tag != "balancer-main" {
		t.Fatalf("balancer tag = %q, want balancer-main", plan.tag)
	}
}

func TestRoutedBalancerPlanIgnoresNonObservableStrategy(t *testing.T) {
	routerConfig := &corerouter.Config{
		Rule: []*corerouter.RoutingRule{{
			TargetTag: &corerouter.RoutingRule_BalancingTag{BalancingTag: "balancer-main"},
		}},
		BalancingRule: []*corerouter.BalancingRule{{
			Tag:              "balancer-main",
			OutboundSelector: []string{"proxy-policy-"},
			Strategy:         "random",
		}},
	}
	config := &core.Config{App: []*serial.TypedMessage{
		serial.ToTypedMessage(routerConfig),
		serial.ToTypedMessage(&coreobservatory.Config{SubjectSelector: []string{"proxy-policy-"}}),
	}}

	plan, err := routedBalancerPlanForConfig(config)
	if err != nil {
		t.Fatalf("routedBalancerPlanForConfig returned an error: %v", err)
	}
	if plan.tag != "" || plan.selectors != nil {
		t.Fatalf("plan = %#v, want empty plan", plan)
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

func TestOutboundDelayResultJSON(t *testing.T) {
	payload, err := json.Marshal(outboundDelayResult{Delay: 42, OutboundTag: "proxy-policy-a"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(payload), `{"delay":42,"outboundTag":"proxy-policy-a"}`; got != want {
		t.Fatalf("payload = %s, want %s", got, want)
	}
}

func TestObservationResultDeadlineFollowsProbeDeadline(t *testing.T) {
	got, err := observationResultDeadlineForProbe(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if want := 32 * time.Second; got != want {
		t.Fatalf("observation result deadline = %v, want %v", got, want)
	}
	if _, err := observationResultDeadlineForProbe(0); err == nil {
		t.Fatal("expected an error for an invalid probe deadline")
	}
}
