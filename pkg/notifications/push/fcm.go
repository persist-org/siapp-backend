package push

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	fcmScope       = "https://www.googleapis.com/auth/firebase.messaging"
	fcmBaseURL     = "https://fcm.googleapis.com"
	tokenLifetime  = time.Hour
	tokenSkew      = time.Minute
	sendConcurrent = 32
	maxAnswerBody  = 4 << 10
)

// serviceAccount is the part of the Firebase service account key file that matters here.
type serviceAccount struct {
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// FCMProvider sends through the FCM HTTP v1 API (iOS goes through APNs behind it).
//
// v1 has no multicast call: one request carries one message. Send runs them concurrently, and
// the worker calls it in chunks of 500, the size the old multicast limit set.
type FCMProvider struct {
	account serviceAccount
	key     *rsa.PrivateKey
	client  *http.Client
	baseURL string

	mu      sync.Mutex
	token   string
	expires time.Time
}

// FCMOption changes an FCMProvider; tests use them to point it at a local server.
type FCMOption func(*FCMProvider)

// WithHTTPClient replaces the HTTP client.
func WithHTTPClient(c *http.Client) FCMOption { return func(p *FCMProvider) { p.client = c } }

// WithBaseURL replaces https://fcm.googleapis.com.
func WithBaseURL(u string) FCMOption {
	return func(p *FCMProvider) { p.baseURL = strings.TrimRight(u, "/") }
}

// WithTokenURI replaces the account's OAuth token endpoint.
func WithTokenURI(u string) FCMOption { return func(p *FCMProvider) { p.account.TokenURI = u } }

// NewFCMProvider builds a provider from FCM_SERVICE_ACCOUNT_BASE64: the service account JSON,
// base64-encoded so it fits in one environment variable.
func NewFCMProvider(serviceAccountBase64 string, opts ...FCMOption) (*FCMProvider, error) {
	// The errors of the decoders can quote the input, which is a secret, so none is wrapped.
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(serviceAccountBase64))
	if err != nil {
		return nil, errors.New("FCM_SERVICE_ACCOUNT_BASE64 is not valid base64")
	}
	p := &FCMProvider{client: &http.Client{Timeout: 15 * time.Second}, baseURL: fcmBaseURL}
	if err := json.Unmarshal(raw, &p.account); err != nil {
		return nil, errors.New("FCM_SERVICE_ACCOUNT_BASE64 does not hold a service account JSON")
	}
	if p.account.ProjectID == "" || p.account.ClientEmail == "" || p.account.PrivateKey == "" {
		return nil, errors.New("FCM service account needs project_id, client_email and private_key")
	}
	if p.account.TokenURI == "" {
		p.account.TokenURI = "https://oauth2.googleapis.com/token"
	}
	p.key, err = jwt.ParseRSAPrivateKeyFromPEM([]byte(p.account.PrivateKey))
	if err != nil {
		return nil, errors.New("FCM service account private_key is not a valid RSA key")
	}
	for _, o := range opts {
		o(p)
	}
	return p, nil
}

// accessToken returns an OAuth token for FCM, asking Google for a new one a minute before the
// cached one runs out (JWT bearer grant, RFC 7523).
func (p *FCMProvider) accessToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if p.token != "" && now.Before(p.expires.Add(-tokenSkew)) {
		return p.token, nil
	}
	assertion, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": p.account.ClientEmail, "scope": fcmScope, "aud": p.account.TokenURI,
		"iat": now.Unix(), "exp": now.Add(tokenLifetime).Unix(),
	}).SignedString(p.key)
	if err != nil {
		return "", fmt.Errorf("sign FCM assertion: %w", err)
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.account.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("FCM token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBody))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("FCM token request: status %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", errors.New("FCM token request: unreadable answer")
	}
	p.token = out.AccessToken
	p.expires = now.Add(time.Duration(out.ExpiresIn) * time.Second)
	return p.token, nil
}

// Send implements Provider.
func (p *FCMProvider) Send(ctx context.Context, msgs []Message) ([]Result, error) {
	token, err := p.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]Result, len(msgs))
	sem := make(chan struct{}, sendConcurrent)
	var wg sync.WaitGroup
	for i := range msgs {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			results[i] = p.sendOne(ctx, token, msgs[i])
		}()
	}
	wg.Wait()
	return results, nil
}

func (p *FCMProvider) sendOne(ctx context.Context, accessToken string, m Message) Result {
	body, err := json.Marshal(map[string]any{"message": buildMessage(m)})
	if err != nil {
		return Result{Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/v1/projects/"+url.PathEscape(p.account.ProjectID)+"/messages:send", bytes.NewReader(body))
	if err != nil {
		return Result{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return Result{Err: fmt.Errorf("FCM send: %w", err)} // the network: worth a retry
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBody))
	if resp.StatusCode == http.StatusOK {
		return Result{}
	}
	if isInvalidToken(resp.StatusCode, raw) {
		return Result{InvalidToken: true}
	}
	err = fmt.Errorf("FCM send: status %d", resp.StatusCode)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// The cached OAuth token was refused (revoked, or the clock drifted): drop it, so the next
		// attempt asks Google for a new one, and let the task retry.
		p.dropAccessToken(accessToken)
		return Result{Err: err}
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == http.StatusRequestTimeout, resp.StatusCode >= 500:
		return Result{Err: err}
	}
	// Every other answer (400 a payload FCM rejects, 403 SENDER_ID_MISMATCH or a disabled API, a
	// 404 that is not UNREGISTERED) is about this message or this configuration, and the same
	// request fails the same way again.
	return Result{Err: err, Permanent: true}
}

// dropAccessToken forgets the cached token, if it is still the one that was refused.
func (p *FCMProvider) dropAccessToken(refused string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token == refused {
		p.token = ""
	}
}

// buildMessage is the FCM v1 message for one device.
func buildMessage(m Message) map[string]any {
	android := map[string]any{"priority": "normal"}
	notification := map[string]any{}
	aps := map[string]any{}
	apnsHeaders := map[string]string{}
	if m.CollapseKey != "" {
		android["collapse_key"] = m.CollapseKey
		notification["tag"] = m.CollapseKey
		apnsHeaders["apns-collapse-id"] = m.CollapseKey
	}
	if m.High {
		android["priority"] = "high"
		notification["channel_id"] = "panic"
		notification["sound"] = "siren"
		apnsHeaders["apns-priority"] = "10"
		// "critical" plays even in silent mode, but only for an app that holds Apple's critical
		// alerts entitlement; without it iOS treats the push as time-sensitive.
		aps["interruption-level"] = "critical"
		aps["sound"] = map[string]any{"critical": 1, "name": "siren.caf", "volume": 1.0}
	}
	android["notification"] = notification
	return map[string]any{
		"token":        m.Token,
		"notification": map[string]string{"title": m.Title, "body": m.Body},
		"data":         m.Data,
		"android":      android,
		"apns":         map[string]any{"headers": apnsHeaders, "payload": map[string]any{"aps": aps}},
	}
}

// isInvalidToken tells a dead token from every other failure, and only by what FCM says about
// the token: the errorCode UNREGISTERED, or an INVALID_ARGUMENT that names the registration
// token. A bare 404 is not enough (a wrong project id or a proxy answers 404 too and would wipe
// every device), and a 400 INVALID_ARGUMENT about the payload must not delete devices either.
func isInvalidToken(status int, raw []byte) bool {
	var e struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	for _, d := range e.Error.Details {
		if d.ErrorCode == "UNREGISTERED" {
			return true
		}
	}
	return status == http.StatusBadRequest && e.Error.Status == "INVALID_ARGUMENT" &&
		strings.Contains(strings.ToLower(e.Error.Message), "registration token")
}
