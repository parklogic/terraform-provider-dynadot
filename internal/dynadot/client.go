// Package dynadot provides the authenticated Dynadot REST client shared by resources.
package dynadot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
)

const ProductionEndpoint = "https://api.dynadot.com/restful/v1"
const SandboxEndpoint = "https://api-sandbox.dynadot.com/restful/v1"

type Nameserver struct {
	Name        string
	IPAddresses []string
}
type Option func(*Client)

func WithEndpoint(endpoint string) Option {
	return func(c *Client) { c.endpoint = strings.TrimRight(endpoint, "/") }
}
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			copyClient := *client
			copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			c.http = &copyClient
		}
	}
}

type Client struct {
	key, endpoint  string
	http           *http.Client
	mu             sync.Mutex
	changed        chan struct{}
	interval       time.Duration
	limit, active  int
	next, cooldown time.Time
	discovered     bool
	discovering    chan struct{}
	tier           string
	recognized     bool
}

func NewClient(apiKey string, sandbox bool, options ...Option) *Client {
	endpoint := ProductionEndpoint
	if sandbox {
		endpoint = SandboxEndpoint
	}
	c := &Client{key: apiKey, endpoint: endpoint, http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, changed: make(chan struct{}), interval: time.Second, limit: 1, tier: "Regular"}
	for _, option := range options {
		option(c)
	}
	return c
}
func (c *Client) signal() { close(c.changed); c.changed = make(chan struct{}) }
func (c *Client) acquire(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		now := time.Now()
		ready := c.next
		if c.cooldown.After(ready) {
			ready = c.cooldown
		}
		if c.active < c.limit && !now.Before(ready) {
			c.active++
			c.next = now.Add(c.interval)
			c.mu.Unlock()
			return nil
		}
		changed := c.changed
		delay := ready.Sub(now)
		capacity := c.active < c.limit
		c.mu.Unlock()
		if !capacity || delay <= 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-changed:
			}
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}
func (c *Client) release() { c.mu.Lock(); c.active--; c.signal(); c.mu.Unlock() }
func (c *Client) deferRequests(header string) {
	delay := 60 * time.Second
	header = strings.TrimSpace(header)
	if header != "" && strings.Trim(header, "0123456789") == "" {
		const maxDelay = time.Duration(1<<63 - 1)
		seconds, err := strconv.ParseUint(header, 10, 64)
		if err != nil || seconds > uint64(maxDelay/time.Second) {
			delay = maxDelay
		} else {
			delay = time.Duration(seconds) * time.Second
		}
	} else if date, err := http.ParseTime(header); err == nil {
		delay = time.Until(date)
		if delay < 0 {
			delay = 0
		}
	}
	c.mu.Lock()
	until := time.Now().Add(delay)
	if until.After(c.cooldown) {
		c.cooldown = until
		c.signal()
	}
	c.mu.Unlock()
}

// todo: update rate limits
func tierLimits(value string) (string, bool, time.Duration, int) {
	value = strings.ToLower(strings.Join(strings.Fields(value), ""))
	value = strings.NewReplacer("-", "", "_", "").Replace(value)
	switch value {
	case "regular":
		return "Regular", true, time.Second, 1
	case "bulk":
		return "Bulk", true, 100 * time.Millisecond, 5
	case "superbulk", "premiumbulkaccount":
		return "SuperBulk", true, 10 * time.Millisecond, 35
	default:
		return "Regular", false, time.Second, 1
	}
}
func (c *Client) Discover(ctx context.Context) (string, bool, error) {
	for {
		c.mu.Lock()
		if c.discovered {
			tier, recognized := c.tier, c.recognized
			c.mu.Unlock()
			return tier, recognized, nil
		}
		if waiting := c.discovering; waiting != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", false, ctx.Err()
			case <-waiting:
			}
			continue
		}
		c.discovering = make(chan struct{})
		c.mu.Unlock()
		break
	}
	var data struct {
		AccountInfo struct {
			PriceLevel string `json:"price_level"`
		} `json:"account_info"`
	}
	err := c.request(ctx, http.MethodGet, "/accounts/info", nil, &data)
	c.mu.Lock()
	defer c.mu.Unlock()
	close(c.discovering)
	c.discovering = nil
	if err != nil {
		return "", false, fmt.Errorf("discover Dynadot account tier: %w", err)
	}
	c.tier, c.recognized, c.interval, c.limit = tierLimits(data.AccountInfo.PriceLevel)
	c.discovered = true
	c.signal()
	return c.tier, c.recognized, nil
}

type APIError struct {
	HTTPStatus, Code int
	Description      string
	notFound         bool
}

func (e *APIError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf(
			"Dynadot request failed (HTTP %d, API code %d): %s",
			e.HTTPStatus,
			e.Code,
			e.Description,
		)
	}
	return fmt.Sprintf("Dynadot request failed (HTTP %d, API code %d)", e.HTTPStatus, e.Code)
}
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.notFound
}

type envelope struct {
	Error struct {
		Description string `json:"description"`
	} `json:"error"`
	Code    *int            `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (c *Client) request(ctx context.Context, method, path string, input, output any) error {
	var payload []byte
	if input != nil {
		var err error
		payload, err = json.Marshal(input)
		if err != nil {
			return errors.New("encode Dynadot request")
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(payload))
		if err != nil {
			return errors.New("invalid Dynadot endpoint")
		}
		req.Header.Set("Authorization", "Bearer "+c.key)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		if err = c.acquire(ctx); err != nil {
			return err
		}
		tflog.Debug(ctx, "Dynadot API request", map[string]any{
			"attempt": attempt + 1,
			"body":    string(payload),
			"headers": headersForLogging(req.Header),
			"method":  method,
			"url":     req.URL.String(),
		})
		response, transportErr := c.http.Do(req)
		if transportErr != nil {
			c.release()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if method != http.MethodPost && attempt < 2 {
				continue
			}
			return errors.New("transport request to Dynadot failed")
		}
		// Publish header-level throttling before a potentially slow body read.
		httpThrottled := response.StatusCode == http.StatusTooManyRequests
		if httpThrottled {
			c.deferRequests(response.Header.Get("Retry-After"))
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		response.Body.Close()
		tflog.Debug(ctx, "Dynadot API response", map[string]any{
			"attempt":     attempt + 1,
			"body":        string(body),
			"headers":     headersForLogging(response.Header),
			"status_code": response.StatusCode,
		})
		var result envelope
		parseErr := json.Unmarshal(body, &result)
		code := 0
		if result.Code != nil && parseErr == nil && readErr == nil {
			code = *result.Code
		}
		throttled := httpThrottled || code == http.StatusTooManyRequests
		if !httpThrottled && code == http.StatusTooManyRequests {
			c.deferRequests(response.Header.Get("Retry-After"))
		}
		c.release()
		if throttled {
			if attempt < 2 {
				continue
			}
			return &APIError{
				HTTPStatus:  response.StatusCode,
				Code:        code,
				Description: strings.TrimSpace(result.Error.Description),
			}
		}
		if response.StatusCode >= 500 && method != http.MethodPost && attempt < 2 {
			continue
		}
		if response.StatusCode != http.StatusNotFound && (readErr != nil || parseErr != nil) {
			return errors.New("invalid Dynadot response envelope")
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 || (result.Code != nil && code != 200) {
			message := strings.TrimSpace(result.Message)
			description := strings.TrimSpace(result.Error.Description)
			absent := response.StatusCode == 404 || code == 404 ||
				strings.EqualFold(message, "Can not find this server in your account") ||
				strings.EqualFold(description, "Can not find this server in your account") ||
				strings.EqualFold(message, "Could not find this nameserver") ||
				strings.EqualFold(description, "Could not find this nameserver")
			return &APIError{
				HTTPStatus:  response.StatusCode,
				Code:        code,
				Description: description,
				notFound:    absent,
			}
		}
		if readErr != nil || parseErr != nil || result.Code == nil {
			return errors.New("invalid Dynadot response envelope")
		}
		if output != nil {
			if len(result.Data) == 0 || string(result.Data) == "null" {
				return errors.New("missing Dynadot response data")
			}
			if json.Unmarshal(result.Data, output) != nil {
				return errors.New("invalid Dynadot response data")
			}
		}
		return nil
	}
	return errors.New("retry limit for Dynadot reached")
}

func headersForLogging(headers http.Header) map[string]any {
	result := make(map[string]any, len(headers))
	for name, values := range headers {
		switch {
		case strings.EqualFold(name, "Authorization"),
			strings.EqualFold(name, "Proxy-Authorization"),
			strings.EqualFold(name, "Cookie"),
			strings.EqualFold(name, "Set-Cookie"):
			result[name] = "[REDACTED]"
		case len(values) == 1:
			result[name] = values[0]
		default:
			result[name] = slices.Clone(values)
		}
	}
	return result
}

func NormalizeHostname(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}
func NormalizeIPs(ips []string) ([]string, error) {
	result := make([]string, 0, len(ips))
	for _, ip := range ips {
		address, err := netip.ParseAddr(strings.TrimSpace(ip))
		if err != nil || address.Zone() != "" {
			return nil, errors.New("invalid nameserver IP address")
		}
		result = append(result, address.Unmap().String())
	}
	slices.Sort(result)
	return slices.Compact(result), nil
}
func (c *Client) GetNameserver(ctx context.Context, name string) (Nameserver, error) {
	var data struct {
		ServerName string   `json:"server_name"`
		IPList     []string `json:"ip_list"`
	}
	name = NormalizeHostname(name)
	err := c.request(ctx, http.MethodGet, "/nameservers/"+url.PathEscape(name), nil, &data)
	if err != nil {
		return Nameserver{}, err
	}
	// Dynadot pads unused IP slots with empty strings in nameserver responses.
	data.IPList = slices.DeleteFunc(data.IPList, func(ip string) bool { return ip == "" })
	ips, err := NormalizeIPs(data.IPList)
	if err != nil || len(ips) == 0 || NormalizeHostname(data.ServerName) != name || name == "" {
		return Nameserver{}, errors.New("invalid Dynadot nameserver data")
	}
	return Nameserver{Name: name, IPAddresses: ips}, nil
}
func (c *Client) RegisterNameserver(ctx context.Context, name, ip string) error {
	return c.request(ctx, http.MethodPost, "/nameservers/register", map[string]any{"name_server": map[string]string{"server_name": NormalizeHostname(name), "ip": ip}}, nil)
}
func (c *Client) SetIPs(ctx context.Context, name string, ips []string) error {
	return c.request(ctx, http.MethodPut, "/nameservers/"+url.PathEscape(NormalizeHostname(name))+"/set_ip", map[string]any{"ip_list": ips}, nil)
}
func (c *Client) DeleteNameserver(ctx context.Context, name string) error {
	return c.request(ctx, http.MethodDelete, "/nameservers/"+url.PathEscape(NormalizeHostname(name)), nil, nil)
}
