// Package ticketclient calls main-api's POST /internal/ssh-ticket from
// ssh-proxy's direct-tcpip handler.
package ticketclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Signed mirrors sshticket.Signed. Duplicated here so ssh-proxy doesn't pull
// main-api internal packages transitively.
type Signed struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// Client posts to /internal/ssh-ticket.
type Client struct {
	BaseURL    string // e.g. http://127.0.0.1:8080
	Token      string // Bearer token matching main-api's MAIN_API_INTERNAL_TOKEN
	HTTPClient *http.Client
}

// New returns a Client with sane HTTP defaults.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: baseURL,
		Token:   token,
		HTTPClient: &http.Client{
			Timeout: 3 * time.Second,
		},
	}
}

// Issue requests a signed ticket for the given subdomain prefix.
func (c *Client) Issue(ctx context.Context, prefix string) (Signed, error) {
	body, _ := json.Marshal(map[string]string{"subdomain_prefix": prefix})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/internal/ssh-ticket", bytes.NewReader(body))
	if err != nil {
		return Signed{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return Signed{}, fmt.Errorf("POST /internal/ssh-ticket: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return Signed{}, fmt.Errorf("ticket status %d: %s", resp.StatusCode, string(b))
	}
	var s Signed
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return Signed{}, fmt.Errorf("decode: %w", err)
	}
	return s, nil
}
