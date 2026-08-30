package maclet

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func readToken(value, file string) (string, error) {
	if value != "" && file != "" {
		return "", errors.New("use only one of --token and --token-file")
	}
	if file != "" {
		var body []byte
		var err error
		if file == "-" {
			body, err = io.ReadAll(os.Stdin)
		} else {
			body, err = os.ReadFile(file)
		}
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		value = string(body)
	}
	if value == "" {
		value = os.Getenv("MACLET_TOKEN")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("cluster token is required (--token-file, --token, or MACLET_TOKEN)")
	}
	return value, nil
}

func tokenPassword(token string) (string, string, error) {
	credentials := token
	caHash := ""
	if strings.HasPrefix(credentials, "K10") {
		if separator := strings.Index(credentials, "::"); separator >= 0 {
			if separator >= 3+64 {
				caHash = credentials[3 : 3+64]
			}
			credentials = credentials[separator+2:]
		}
	}
	if separator := strings.IndexByte(credentials, ':'); separator >= 0 {
		credentials = credentials[separator+1:]
	}
	if credentials == "" {
		return "", caHash, errors.New("cluster token does not contain a password")
	}
	return credentials, caHash, nil
}

func fetchCA(ctx context.Context, server string, expectedHash string) ([]byte, error) {
	base, err := normalizeServer(server)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	client := &http.Client{Timeout: 15 * time.Second, Transport: transport}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base.ResolveReference(&url.URL{Path: "/cacerts"}).String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("get cluster CA: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get cluster CA: HTTP %s", response.Status)
	}
	caPEM, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if _, rest := pem.Decode(caPEM); rest == nil {
		return nil, errors.New("cluster CA response is not PEM")
	}
	if expectedHash != "" {
		digest := sha256.Sum256(caPEM)
		actual := hex.EncodeToString(digest[:])
		if actual != expectedHash {
			return nil, fmt.Errorf("cluster CA hash mismatch: token=%s server=%s", expectedHash, actual)
		}
	}
	return caPEM, nil
}

func randomPassword() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func generateClientCSR(nodeName string) (csrDER, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	requestDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "system:node:" + nodeName},
	}, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return requestDER,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}
