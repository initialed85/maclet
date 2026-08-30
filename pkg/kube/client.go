package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client is the small HTTPS client used by maclet. It deliberately uses
// only the Kubernetes JSON API rather than pulling in client-go: maclet needs a
// narrow, inspectable control-plane surface while its runtime is still being
// designed.
type Client struct {
	base        *url.URL
	http        *http.Client
	username    string
	password    string
	bearerToken string
}

type HTTPError struct {
	Code int
	Body string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("kubernetes API returned HTTP %d", e.Code)
	}
	return fmt.Sprintf("kubernetes API returned HTTP %d: %s", e.Code, e.Body)
}

func NewClient(server string, caPEM []byte, certFile, keyFile string, insecure bool, username, password string) (*Client, error) {
	var certPEM, keyPEM []byte
	if certFile != "" || keyFile != "" {
		var err error
		certPEM, err = os.ReadFile(certFile)
		if err != nil {
			return nil, fmt.Errorf("read client certificate: %w", err)
		}
		keyPEM, err = os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("read client key: %w", err)
		}
	}
	return NewClientWithMaterial(server, caPEM, certPEM, keyPEM, insecure, username, password, "")
}

func NewClientWithMaterial(server string, caPEM, certPEM, keyPEM []byte, insecure bool, username, password, bearerToken string) (*Client, error) {
	base, err := NormalizeServer(server)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if insecure {
		tlsConfig.InsecureSkipVerify = true // explicitly requested for development clusters
	} else {
		pool := x509.NewCertPool()
		if len(caPEM) == 0 || !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("cluster CA bundle is empty or invalid")
		}
		tlsConfig.RootCAs = pool
	}
	if len(certPEM) != 0 || len(keyPEM) != 0 {
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("load client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	return &Client{
		base: base,
		http: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsConfig},
		},
		username:    username,
		password:    password,
		bearerToken: bearerToken,
	}, nil
}

func NormalizeServer(server string) (*url.URL, error) {
	if server == "" {
		return nil, errors.New("server URL is required")
	}
	u, err := url.Parse(strings.TrimRight(server, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse server URL: %w", err)
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("server URL must use https:// (got %q)", server)
	}
	if u.Host == "" {
		return nil, errors.New("server URL has no host")
	}
	return u, nil
}

func (c *Client) endpoint(path string) string {
	u := *c.base
	p, err := url.Parse(path)
	if err != nil {
		return ""
	}
	u.Path = p.Path
	u.RawQuery = p.RawQuery
	u.Fragment = ""
	return u.String()
}

func (c *Client) Do(ctx context.Context, method, path string, body []byte, contentType string, headers map[string]string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if c.bearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+c.bearerToken)
	} else if c.username != "" {
		request.SetBasicAuth(c.username, c.password)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("Accept", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &HTTPError{Code: response.StatusCode, Body: strings.TrimSpace(string(responseBody))}
	}
	return responseBody, nil
}

func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	return c.Do(ctx, http.MethodGet, path, nil, "", nil)
}

func (c *Client) PostJSON(ctx context.Context, path string, value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return c.Do(ctx, http.MethodPost, path, body, "application/json", nil)
}

func (c *Client) PutJSON(ctx context.Context, path string, value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return c.Do(ctx, http.MethodPut, path, body, "application/json", nil)
}

func (c *Client) PatchJSON(ctx context.Context, path string, value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return c.Do(ctx, http.MethodPatch, path, body, "application/merge-patch+json", nil)
}

func (c *Client) Delete(ctx context.Context, path string) ([]byte, error) {
	return c.Do(ctx, http.MethodDelete, path, nil, "", nil)
}
