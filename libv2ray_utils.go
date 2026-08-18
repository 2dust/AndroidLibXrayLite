package libv2ray

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	corenet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	coresession "github.com/xtls/xray-core/common/session"
	core "github.com/xtls/xray-core/core"
	corerouting "github.com/xtls/xray-core/features/routing"
	corestats "github.com/xtls/xray-core/features/stats"
	coreserial "github.com/xtls/xray-core/infra/conf/serial"
)

// QueryAllOutboundTrafficStats retrieves and resets all outbound traffic counters.
// Returns a single-line text in format: tag,direction,value;tag,direction,value;
// Returns an empty string if the stats manager is not initialized or no counters exist.
func (x *CoreController) QueryAllOutboundTrafficStats() string {
	if x.statsManager == nil {
		return ""
	}

	var b strings.Builder

	x.statsManager.VisitCounters(func(name string, counter corestats.Counter) bool {
		parts := strings.Split(name, ">>>")
		if len(parts) != 4 || parts[0] != "outbound" || parts[2] != "traffic" {
			return true
		}

		tag := parts[1]
		direct := parts[3]
		value := counter.Set(0)
		if value <= 0 {
			return true
		}

		b.WriteString(tag)
		b.WriteByte(',')
		b.WriteString(direct)
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(value, 10))
		b.WriteByte(';')
		return true
	})
	return b.String()
}

// MeasureDelay measures network latency to a specified URL through the current core instance
// Uses a 12-second timeout context and returns the round-trip time in milliseconds
// An error is returned if the connection fails or returns an unexpected status
func (x *CoreController) MeasureDelay(url string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	return measureInstDelay(ctx, x.coreInstance, url)
}

// GetBalancerPrincipleTarget returns the strategy's current first-choice
// outbound. An empty result means the observatory has not produced a viable
// target yet or the running profile has no compatible balancer.
func (x *CoreController) GetBalancerPrincipleTarget(balancerTag string) (string, error) {
	x.coreMutex.Lock()
	defer x.coreMutex.Unlock()

	if !x.IsRunning || x.coreInstance == nil {
		return "", nil
	}
	return firstBalancerPrincipleTarget(x.coreInstance, balancerTag)
}

func firstBalancerPrincipleTarget(inst *core.Instance, balancerTag string) (string, error) {
	if balancerTag == "" {
		return "", nil
	}
	if inst == nil {
		return "", errors.New("core instance is nil")
	}
	principle, ok := inst.GetFeature(corerouting.RouterType()).(corerouting.BalancerPrincipleTarget)
	if !ok {
		return "", errors.New("router does not expose balancer principle targets")
	}
	targets, err := principle.GetPrincipleTarget(balancerTag)
	if err != nil {
		return "", err
	}
	for _, target := range targets {
		if target != "" {
			return target, nil
		}
	}
	return "", nil
}

// GetUrlContent retrieves a URL through the requested outbound of the current core instance.
func (x *CoreController) GetUrlContent(url string, outboundTag string) (string, error) {
	if x.coreInstance == nil {
		return "", errors.New("core instance is nil")
	}
	if outboundTag == "" {
		return "", errors.New("outbound tag is empty")
	}

	tr := &http.Transport{
		TLSHandshakeTimeout: 5 * time.Second,
		DisableKeepAlives:   true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dest, err := corenet.ParseDestination(fmt.Sprintf("%s:%s", network, addr))
			if err != nil {
				return nil, err
			}
			ctx = coresession.SetForcedOutboundTagToContext(ctx, outboundTag)
			return core.Dial(ctx, x.coreInstance, dest)
		},
	}
	defer tr.CloseIdleConnections()

	resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("invalid status: %s", resp.Status)
	}

	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}
	return string(content), nil
}

// MeasureOutboundDelay measures the outbound delay for a given configuration and URL
func MeasureOutboundDelay(ConfigureFileContent string, url string) (int64, error) {
	config, err := coreserial.LoadJSONConfig(strings.NewReader(ConfigureFileContent))
	if err != nil {
		return -1, fmt.Errorf("config load error: %w", err)
	}

	config.Inbound = nil
	var essentialApp []*serial.TypedMessage
	for _, app := range config.App {
		if app.Type == "xray.app.proxyman.OutboundConfig" ||
			app.Type == "xray.app.dispatcher.Config" ||
			app.Type == "xray.app.log.Config" {
			essentialApp = append(essentialApp, app)
		}
	}
	config.App = essentialApp

	inst, err := core.New(config)
	if err != nil {
		return -1, fmt.Errorf("instance creation failed: %w", err)
	}

	if err := inst.Start(); err != nil {
		return -1, fmt.Errorf("startup failed: %w", err)
	}
	defer inst.Close()
	return measureInstDelay(context.Background(), inst, url)
}

// measureInstDelay measures the delay for an instance to a given URL
func measureInstDelay(ctx context.Context, inst *core.Instance, url string) (int64, error) {
	if inst == nil {
		return -1, errors.New("core instance is nil")
	}

	if url == "" {
		url = "https://www.google.com/generate_204"
	}

	tr := &http.Transport{
		TLSHandshakeTimeout: 6 * time.Second,
		DisableKeepAlives:   false,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dest, err := corenet.ParseDestination(fmt.Sprintf("%s:%s", network, addr))
			if err != nil {
				return nil, err
			}
			return core.Dial(ctx, inst, dest)
		},
	}

	client := &http.Client{
		Transport: tr,
		Timeout:   12 * time.Second,
	}

	var minDuration int64 = -1
	success := false
	var lastErr error

	defer tr.CloseIdleConnections()

	const attempts = 2
	for i := 0; i < attempts; i++ {
		select {
		case <-ctx.Done():
			if !success {
				return -1, ctx.Err()
			}
			return minDuration, nil
		default:
		}

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			lastErr = fmt.Errorf("failed to create HTTP request: %w", err)
			continue
		}

		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		_, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			lastErr = fmt.Errorf("invalid status: %s", resp.Status)
			continue
		}

		if err != nil {
			lastErr = fmt.Errorf("failed to read response body: %w", err)
			continue
		}

		duration := time.Since(start).Milliseconds()
		if !success || duration < minDuration {
			minDuration = duration
		}

		success = true
	}
	if !success {
		return -1, lastErr
	}
	return minDuration, nil
}
