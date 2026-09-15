// Package webhook delivers completed submission results to caller-supplied
// URLs.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client posts result payloads to submission webhook URLs.
type Client struct {
	http *http.Client
}

// New builds a webhook client with the given per-attempt timeout.
func New(timeout time.Duration) *Client {
	return &Client{
		http: &http.Client{
			Timeout: timeout,
			// Redirects are not followed: a caller-controlled URL that
			// redirects is a straightforward way to smuggle a request to an
			// address that failed the checks in Validate.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Deliver POSTs body to rawURL. A non-nil error means the attempt should be
// retried; delivery is considered successful on any 2xx.
func (c *Client) Deliver(ctx context.Context, rawURL string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal webhook body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ocee-webhook/1")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("post webhook: %w", err)
	}
	defer resp.Body.Close()
	// Drain a little so the connection can be reused, but do not read an
	// unbounded response from an endpoint we do not control.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode/100 == 2 {
		return nil
	}
	return fmt.Errorf("webhook returned %s", resp.Status)
}

// Validate rejects webhook URLs that are malformed or point somewhere the
// engine should not be made to reach.
//
// This is SSRF defence: the caller picks this URL, and the worker sits inside
// the compose network with a route to Postgres, Redis, and the Docker socket
// host. Blocking private and loopback destinations keeps a submission from
// using the webhook as a probe into that network.
//
// allowPrivate disables the address check. It exists so webhooks can be
// exercised against a receiver on the developer's own machine, which the check
// otherwise makes impossible. It must stay false anywhere the API is reachable
// by someone who is not the operator.
func Validate(rawURL string, allowPrivate bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("webhook_url is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("webhook_url must use http or https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("webhook_url is missing a host")
	}
	if allowPrivate {
		return nil
	}

	// A hostname is resolved here only to reject obviously-internal targets.
	// This is a best-effort check, not a guarantee: DNS can return something
	// different at delivery time (a DNS-rebinding race). Closing that hole
	// properly needs a dialer-level check on the resolved IP, which is noted
	// as a known limitation in the README.
	ips, err := net.LookupIP(host)
	if err != nil {
		// An unresolvable host is not fatal at submit time; the delivery
		// attempt will simply fail and retry.
		if strings.EqualFold(host, "localhost") {
			return fmt.Errorf("webhook_url may not target localhost")
		}
		return nil
	}
	for _, ip := range ips {
		if isInternal(ip) {
			return fmt.Errorf("webhook_url may not target a private or loopback address")
		}
	}
	return nil
}

func isInternal(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	// 169.254.169.254 is covered by the link-local check above, but carrier
	// NAT space is not private per RFC 1918 and is still not a valid target.
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1]&0xC0 == 64 {
		return true
	}
	return false
}
