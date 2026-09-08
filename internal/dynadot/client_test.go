package dynadot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflogtest"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func testClient(f roundTripFunc) *Client {
	return NewClient("secret-key", false, WithHTTPClient(&http.Client{Transport: f}))
}

func TestContracts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		c := testClient(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") != "Bearer secret-key" || r.URL.Host != "api.dynadot.com" {
				t.Errorf("bad auth or host")
			}
			methods := []string{"GET", "POST", "PUT", "GET", "DELETE"}
			paths := []string{"/accounts/info", "/nameservers/register", "/nameservers/ns.example.com/set_ip", "/nameservers/ns.example.com", "/nameservers/ns.example.com"}
			if r.Method != methods[calls] || r.URL.Path != "/restful/v1"+paths[calls] {
				t.Errorf("request %d: %s %s", calls, r.Method, r.URL.Path)
			}
			body, _ := io.ReadAll(r.Body)
			if calls == 1 && string(body) != `{"name_server":{"ip":"192.0.2.1","server_name":"ns.example.com"}}` {
				t.Errorf("register: %s", body)
			}
			if calls == 2 && string(body) != `{"ip_list":["192.0.2.1","2001:db8::1"]}` {
				t.Errorf("set: %s", body)
			}
			if calls == 4 && r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("delete Content-Type: %q", r.Header.Get("Content-Type"))
			}
			calls++
			switch calls {
			case 1:
				return reply(200, `{"code":200,"data":{"account_info":{"price_level":"Bulk"}}}`), nil
			case 4:
				return reply(200, `{"code":200,"data":{"server_name":"NS.EXAMPLE.COM.","ip_list":["2001:0db8::1","192.0.2.1","192.0.2.1"]}}`), nil
			}
			return reply(200, `{"code":200}`), nil
		})
		if tier, ok, err := c.Discover(t.Context()); err != nil || !ok || tier != "Bulk" {
			t.Fatalf("discovery %s %v %v", tier, ok, err)
		}
		if _, _, err := c.Discover(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := c.RegisterNameserver(t.Context(), "NS.EXAMPLE.COM.", "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
		if err := c.SetIPs(t.Context(), "ns.example.com", []string{"192.0.2.1", "2001:db8::1"}); err != nil {
			t.Fatal(err)
		}
		ns, err := c.GetNameserver(t.Context(), "ns.example.com")
		if err != nil || ns.Name != "ns.example.com" || strings.Join(ns.IPAddresses, ",") != "192.0.2.1,2001:db8::1" {
			t.Fatalf("nameserver %+v %v", ns, err)
		}
		if err := c.DeleteNameserver(t.Context(), ns.Name); err != nil {
			t.Fatal(err)
		}
		if calls != 5 {
			t.Fatalf("calls %d", calls)
		}
	})
}
func TestGetNameserverPaddedIPs(t *testing.T) {
	for _, tt := range []struct {
		name, ips, want string
		wantError       bool
	}{
		{"padded", `["192.0.2.10","198.51.100.34","203.0.113.36","","","","","","",""]`, "192.0.2.10,198.51.100.34,203.0.113.36", false},
		{"interspersed", `["","2001:0db8::1","","192.0.2.1","192.0.2.1",""]`, "192.0.2.1,2001:db8::1", false},
		{"all_empty", `["",""]`, "", true},
		{"invalid_nonempty", `["192.0.2.1","","invalid"]`, "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(func(*http.Request) (*http.Response, error) {
				return reply(200, `{"code":200,"data":{"server_name":"ns.example.com","ip_list":`+tt.ips+`},"message":"Success"}`), nil
			})
			ns, err := c.GetNameserver(t.Context(), "ns.example.com")
			if (err != nil) != tt.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tt.wantError && (ns.Name != "ns.example.com" || strings.Join(ns.IPAddresses, ",") != tt.want) {
				t.Fatalf("unexpected nameserver: %+v", ns)
			}
		})
	}
}

func TestDiscoverTiers(t *testing.T) {
	for _, tt := range []struct {
		name, value, tier string
		recognized        bool
		interval          time.Duration
		limit             int
	}{{"regular", " Regular ", "Regular", true, time.Second, 1}, {"bulk", " bUlK ", "Bulk", true, 100 * time.Millisecond, 5}, {"super", " SUPER_bulk- ", "SuperBulk", true, 10 * time.Millisecond, 35}, {"premium", "Premium Bulk Account", "SuperBulk", true, 10 * time.Millisecond, 35}, {"unknown", "enterprise", "Regular", false, time.Second, 1}, {"missing", "", "Regular", false, time.Second, 1}} {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(func(*http.Request) (*http.Response, error) {
				body, _ := json.Marshal(map[string]any{"code": 200, "data": map[string]any{"account_info": map[string]string{"price_level": tt.value}}})
				return reply(200, string(body)), nil
			})
			tier, ok, err := c.Discover(t.Context())
			if err != nil || tier != tt.tier || ok != tt.recognized || c.interval != tt.interval || c.limit != tt.limit {
				t.Fatalf("%s %v %v", tier, ok, err)
			}
		})
	}
}
func TestErrors(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
		absent     bool
	}{{"explicit", `{"code":404}`, 200, true}, {"documented", `{"code":400,"message":"Bad Request","error":{"description":"Can not find this server in your account"}}`, 200, true}, {"generic400", `{"code":400,"message":"secret-key"}`, 400, false}, {"auth", `{"code":401,"message":"secret-key"}`, 401, false}, {"malformed", "secret-key", 200, false}, {"missingcode", `{"data":{}}`, 200, false}, {"missingdata", `{"code":200}`, 200, false}, {"partial", `{"code":200,"data":{"server_name":"ns.example.com"}}`, 200, false}, {"invalidip", `{"code":200,"data":{"server_name":"ns.example.com","ip_list":["secret-key"]}}`, 200, false}, {"mismatch", `{"code":200,"data":{"server_name":"other","ip_list":["192.0.2.1"]}}`, 200, false}} {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(func(*http.Request) (*http.Response, error) { return reply(tt.status, tt.body), nil })
			_, err := c.GetNameserver(t.Context(), "ns.example.com")
			if err == nil || IsNotFound(err) != tt.absent || strings.Contains(err.Error(), "secret-key") {
				t.Fatalf("error %v", err)
			}
		})
	}
}

func TestAPIErrorIncludesDescription(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return reply(http.StatusBadRequest, `{"code":400,"message":"Bad Request","error":{"description":"Unsupported content-type in the header."}}`), nil
	})

	err := c.DeleteNameserver(t.Context(), "ns.example.com")
	if err == nil || !strings.Contains(err.Error(), "Unsupported content-type in the header.") {
		t.Fatalf("error does not include API description: %v", err)
	}
}

func TestRequestAndResponseDebugLogs(t *testing.T) {
	var logs bytes.Buffer
	ctx := tflogtest.RootLogger(t.Context(), &logs)
	c := testClient(func(*http.Request) (*http.Response, error) {
		response := reply(http.StatusBadRequest, `{"code":400,"message":"Bad Request","error":{"description":"invalid request"}}`)
		response.Header.Set("Content-Type", "application/json")
		return response, nil
	})

	_ = c.SetIPs(ctx, "ns.example.com", []string{"192.0.2.1"})
	entries, err := tflogtest.MultilineJSONDecode(&logs)
	if err != nil {
		t.Fatalf("decode debug logs: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("debug entries = %d, want 2: %s", len(entries), logs.String())
	}
	request := entries[0]
	if request["@message"] != "Dynadot API request" || request["method"] != http.MethodPut || request["url"] != ProductionEndpoint+"/nameservers/ns.example.com/set_ip" || request["body"] != `{"ip_list":["192.0.2.1"]}` {
		t.Fatalf("unexpected request log: %#v", request)
	}
	response := entries[1]
	if response["@message"] != "Dynadot API response" || response["status_code"] != float64(http.StatusBadRequest) || response["body"] != `{"code":400,"message":"Bad Request","error":{"description":"invalid request"}}` {
		t.Fatalf("unexpected response log: %#v", response)
	}
	if strings.Contains(logs.String(), "secret-key") {
		t.Fatalf("debug logs exposed API key: %s", logs.String())
	}
}
func TestDiscoveryFailureAndUnsafeRetry(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				c := testClient(func(*http.Request) (*http.Response, error) {
					calls++
					return nil, errors.New("secret-key in transport URL")
				})
				var err error
				if method == "GET" {
					_, _, err = c.Discover(t.Context())
				} else {
					err = c.RegisterNameserver(t.Context(), "ns.example.com", "192.0.2.1")
				}
				want := 3
				if method == "POST" {
					want = 1
				}
				if err == nil || strings.Contains(err.Error(), "secret-key") || calls != want || c.discovered {
					t.Fatalf("calls %d error %v", calls, err)
				}
			})
		})
	}
}
func TestPacingAndConcurrency(t *testing.T) {
	for _, tier := range []string{"Regular", "Bulk", "SuperBulk"} {
		t.Run(tier, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, _, interval, limit := tierLimits(tier)
				var mu sync.Mutex
				var starts []time.Time
				active, maxActive := 0, 0
				c := testClient(func(*http.Request) (*http.Response, error) {
					mu.Lock()
					starts = append(starts, time.Now())
					active++
					if active > maxActive {
						maxActive = active
					}
					mu.Unlock()
					time.Sleep(time.Second)
					mu.Lock()
					active--
					mu.Unlock()
					return reply(200, `{"code":200}`), nil
				})
				c.interval = interval
				c.limit = limit
				var wg sync.WaitGroup
				for range limit + 5 {
					wg.Go(func() {
						if err := c.DeleteNameserver(t.Context(), "ns.example.com"); err != nil {
							t.Error(err)
						}
					})
				}
				wg.Wait()
				for i := 1; i < len(starts); i++ {
					if starts[i].Sub(starts[i-1]) < interval {
						t.Fatalf("burst at %d", i)
					}
				}
				if maxActive > limit || maxActive < limit {
					t.Fatalf("max active %d want %d", maxActive, limit)
				}
				time.Sleep(time.Minute)
				before := time.Now()
				if err := c.DeleteNameserver(t.Context(), "ns.example.com"); err != nil {
					t.Fatal(err)
				}
				if time.Since(before) != time.Second {
					t.Fatal("idle start delayed")
				}
			})
		})
	}
}
func TestSharedCooldownAndCancellation(t *testing.T) {
	for _, tt := range []struct {
		name, header string
		api          bool
		wait         time.Duration
	}{{"seconds", "3", false, 3 * time.Second}, {"date", "", false, 4 * time.Second}, {"default", "invalid", true, time.Minute}} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				var starts []time.Time
				c := testClient(func(*http.Request) (*http.Response, error) {
					calls++
					starts = append(starts, time.Now())
					if calls == 1 {
						r := reply(429, `{"code":429}`)
						if tt.api {
							r.StatusCode = 200
						}
						header := tt.header
						if tt.name == "date" {
							header = time.Now().Add(tt.wait).Format(http.TimeFormat)
						}
						r.Header.Set("Retry-After", header)
						return r, nil
					}
					return reply(200, `{"code":200}`), nil
				})
				done := make(chan error, 1)
				go func() { done <- c.DeleteNameserver(t.Context(), "ns.example.com") }()
				synctest.Wait()
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				err := c.DeleteNameserver(ctx, "another.example.com")
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("cancel %v", err)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if len(starts) != 2 || starts[1].Sub(starts[0]) < tt.wait {
					t.Fatalf("cooldown calls %v", starts)
				}
			})
		})
	}
}
func TestRegistrationServerFailureNotRetried(t *testing.T) {
	calls := 0
	c := testClient(func(*http.Request) (*http.Response, error) { calls++; return reply(503, `{"code":503}`), nil })
	if err := c.RegisterNameserver(t.Context(), "ns.example.com", "192.0.2.1"); err == nil || calls != 1 {
		t.Fatalf("calls %d error %v", calls, err)
	}
}

func TestConcurrentDiscoveryCached(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		c := testClient(func(*http.Request) (*http.Response, error) {
			calls++
			time.Sleep(time.Second)
			return reply(200, `{"code":200,"data":{"account_info":{"price_level":"SuperBulk"}}}`), nil
		})
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				tier, ok, err := c.Discover(t.Context())
				if tier != "SuperBulk" || !ok || err != nil {
					t.Errorf("discovery %s %v %v", tier, ok, err)
				}
			})
		}
		wg.Wait()
		if calls != 1 {
			t.Fatalf("discovery calls %d", calls)
		}
	})
}
func TestRetryBoundAndPacing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, status := range []int{429, 503} {
			calls := 0
			start := time.Now()
			c := testClient(func(*http.Request) (*http.Response, error) {
				calls++
				r := reply(status, `{"code":503}`)
				r.Header.Set("Retry-After", "0")
				return r, nil
			})
			if err := c.DeleteNameserver(t.Context(), "ns.example.com"); err == nil || calls != 3 || time.Since(start) != 2*time.Second {
				t.Fatalf("retry status %d calls %d elapsed %s err %v", status, calls, time.Since(start), err)
			}
		}
	})
}
func TestCancellationWaitingForCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		finish := make(chan struct{})
		c := testClient(func(*http.Request) (*http.Response, error) {
			close(started)
			<-finish
			return reply(200, `{"code":200}`), nil
		})
		done := make(chan error, 1)
		go func() { done <- c.DeleteNameserver(t.Context(), "ns.example.com") }()
		<-started
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := c.DeleteNameserver(ctx, "other.example.com"); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel %v", err)
		}
		close(finish)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}
func TestEndpointAndRedirectPolicy(t *testing.T) {
	c := NewClient("key", true)
	if c.endpoint != SandboxEndpoint {
		t.Fatal(c.endpoint)
	}
	supplied := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	c = NewClient("key", false, WithHTTPClient(supplied))
	if !errors.Is(c.http.CheckRedirect(nil, nil), http.ErrUseLastResponse) || supplied.CheckRedirect(nil, nil) != nil {
		t.Fatal("redirect policy must prevent credential forwarding without mutating supplied client")
	}
}
func TestNormalizeIPs(t *testing.T) {
	ips, err := NormalizeIPs([]string{" 2001:0db8::1 ", "::ffff:192.0.2.1", "192.0.2.1"})
	if err != nil || strings.Join(ips, ",") != "192.0.2.1,2001:db8::1" {
		t.Fatalf("%v %v", ips, err)
	}
	for _, value := range []string{"", "invalid", "fe80::1%eth0", "192.0.2.1/24"} {
		if _, err := NormalizeIPs([]string{value}); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

type blockedBody struct {
	ready   chan struct{}
	unblock <-chan struct{}
	reader  io.Reader
}

func (b *blockedBody) Read(p []byte) (int, error) {
	if b.ready != nil {
		close(b.ready)
		b.ready = nil
		<-b.unblock
	}
	return b.reader.Read(p)
}
func (b *blockedBody) Close() error { return nil }

func TestHTTPCooldownStartsBeforeBodyRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bodyStarted := make(chan struct{})
		unblockBody := make(chan struct{})
		var starts []time.Time
		c := testClient(func(*http.Request) (*http.Response, error) {
			starts = append(starts, time.Now())
			if len(starts) == 1 {
				r := reply(429, "")
				r.Header.Set("Retry-After", "10")
				r.Body = &blockedBody{ready: bodyStarted, unblock: unblockBody, reader: strings.NewReader(`{"code":429}`)}
				return r, nil
			}
			return reply(200, `{"code":200}`), nil
		})
		c.interval = 100 * time.Millisecond
		c.limit = 5
		first := make(chan error, 1)
		go func() { first <- c.DeleteNameserver(t.Context(), "ns.example.com") }()
		<-bodyStarted
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		if err := c.DeleteNameserver(ctx, "other.example.com"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("request during slow 429 body: %v", err)
		}
		if len(starts) != 1 {
			t.Fatalf("transport started during cooldown: %d calls", len(starts))
		}
		close(unblockBody)
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		if len(starts) != 2 || starts[1].Sub(starts[0]) != 10*time.Second {
			t.Fatalf("cooldown must start once at headers; starts %v", starts)
		}
	})
}

func TestLongRetryAfter(t *testing.T) {
	for _, tt := range []struct {
		name, header string
		delay        time.Duration
	}{
		{"over_one_day", "86401", 86401 * time.Second},
		{"duration_overflow", "9223372037", time.Duration(1<<63 - 1)},
		{"uint64_overflow", "18446744073709551616", time.Duration(1<<63 - 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				c := testClient(func(*http.Request) (*http.Response, error) {
					calls++
					r := reply(429, `{"code":429}`)
					r.Header.Set("Retry-After", tt.header)
					return r, nil
				})
				start := time.Now()
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				defer cancel()
				if err := c.DeleteNameserver(ctx, "ns.example.com"); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("cooldown cancellation: %v", err)
				}
				if calls != 1 {
					t.Fatalf("retried before server cooldown: %d calls", calls)
				}
				if got := c.cooldown.Sub(start); got != tt.delay {
					t.Fatalf("cooldown %s want %s", got, tt.delay)
				}
			})
		})
	}
}

func TestMalformedAbsenceIsNotNotFound(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
		absent     bool
	}{
		{"partial_code", `{"code":404,"message":{}}`, 200, false},
		{"partial_description", `{"code":400,"message":"Can not find this server in your account","error":[]}`, 200, false},
		{"http_bad_request", `{"code":404,"message":{}}`, 400, false},
		{"explicit_http_absence", `{"code":404,"message":{}}`, 404, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(func(*http.Request) (*http.Response, error) { return reply(tt.status, tt.body), nil })
			_, err := c.GetNameserver(t.Context(), "ns.example.com")
			if err == nil || IsNotFound(err) != tt.absent {
				t.Fatalf("malformed response absence=%v: %v", IsNotFound(err), err)
			}
		})
	}
}

type failedBody struct{}

func (failedBody) Read(p []byte) (int, error) {
	return copy(p, `{"code":404}`), errors.New("response body interrupted")
}
func (failedBody) Close() error { return nil }
func TestInterruptedBodyIsNotNotFound(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) { r := reply(200, ""); r.Body = failedBody{}; return r, nil })
	_, err := c.GetNameserver(t.Context(), "ns.example.com")
	if err == nil || IsNotFound(err) {
		t.Fatalf("interrupted body treated as absence: %v", err)
	}
}
