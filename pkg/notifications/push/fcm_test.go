package push_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/notifications/push"
)

func serviceAccountB64(t *testing.T, tokenURI string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	raw, err := json.Marshal(map[string]string{
		"project_id": "demo", "client_email": "svc@demo.iam", "private_key": string(pemKey), "token_uri": tokenURI,
	})
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(raw)
}

// fcmServer answers the OAuth exchange and the send endpoint. The answer of a send is chosen by
// the token in the message.
type fcmServer struct {
	*httptest.Server
	mu       sync.Mutex
	messages []map[string]any
	exchange atomic.Int32
}

func newFCMServer(t *testing.T) *fcmServer {
	t.Helper()
	s := &fcmServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			s.exchange.Add(1)
			require.NoError(t, r.ParseForm())
			require.Equal(t, "urn:ietf:params:oauth:grant-type:jwt-bearer", r.Form.Get("grant_type"))
			require.Equal(t, 3, strings.Count(r.Form.Get("assertion"), ".")+1, "a signed JWT")
			_, _ = io.WriteString(w, `{"access_token":"ya29.fake","expires_in":3600}`)
		case "/v1/projects/demo/messages:send":
			require.Equal(t, "Bearer ya29.fake", r.Header.Get("Authorization"))
			var body struct {
				Message map[string]any `json:"message"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			s.mu.Lock()
			s.messages = append(s.messages, body.Message)
			s.mu.Unlock()
			switch body.Message["token"] {
			case "dead":
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":{"code":404,"status":"NOT_FOUND","details":[{"errorCode":"UNREGISTERED"}]}}`)
			case "garbage":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"The registration token is not a valid FCM registration token"}}`)
			case "badpayload":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"Invalid value at message.data"}}`)
			case "down":
				w.WriteHeader(http.StatusServiceUnavailable)
			case "throttled":
				w.WriteHeader(http.StatusTooManyRequests)
			case "bare404":
				// What a wrong project id or a proxy answers: not FCM saying the token is dead.
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":{"code":404,"status":"NOT_FOUND","message":"Requested entity was not found."}}`)
			case "forbidden":
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"errorCode":"SENDER_ID_MISMATCH"}]}}`)
			case "unauthorized":
				w.WriteHeader(http.StatusUnauthorized)
			default:
				_, _ = io.WriteString(w, `{"name":"projects/demo/messages/1"}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *fcmServer) provider(t *testing.T) *push.FCMProvider {
	t.Helper()
	p, err := push.NewFCMProvider(serviceAccountB64(t, s.URL+"/token"), push.WithBaseURL(s.URL))
	require.NoError(t, err)
	return p
}

func TestFCMClassifiesEveryAnswer(t *testing.T) {
	s := newFCMServer(t)
	p := s.provider(t)

	msgs := []push.Message{
		{Token: "good", Title: "t", Body: "b", Data: map[string]string{"type": "X"}},
		{Token: "dead", Title: "t", Body: "b"},
		{Token: "garbage", Title: "t", Body: "b"},
		{Token: "badpayload", Title: "t", Body: "b"},
		{Token: "down", Title: "t", Body: "b"},
	}
	results, err := p.Send(t.Context(), msgs)
	require.NoError(t, err)
	require.Len(t, results, len(msgs))

	require.Equal(t, push.Result{}, results[0])
	require.True(t, results[1].InvalidToken, "UNREGISTERED")
	require.True(t, results[2].InvalidToken, "INVALID_ARGUMENT that names the token")
	require.False(t, results[3].InvalidToken, "a payload problem must not delete devices")
	require.Error(t, results[3].Err)
	require.True(t, results[3].Permanent, "and it will not pass by retrying")
	require.False(t, results[4].InvalidToken)
	require.Error(t, results[4].Err, "a 503 is retried")
	require.False(t, results[4].Permanent)
	require.NotContains(t, results[4].Err.Error(), "down", "the token stays out of errors")
}

func TestFCMTellsAPermanentFailureFromOneThatMayPass(t *testing.T) {
	s := newFCMServer(t)
	p := s.provider(t)

	tokens := []string{"throttled", "down", "bare404", "forbidden", "badpayload"}
	msgs := make([]push.Message, len(tokens))
	for i, tok := range tokens {
		msgs[i] = push.Message{Token: tok}
	}
	results, err := p.Send(t.Context(), msgs)
	require.NoError(t, err)

	for i, tok := range tokens {
		require.False(t, results[i].InvalidToken, "%s must not delete a device", tok)
		require.Error(t, results[i].Err, tok)
	}
	require.False(t, results[0].Permanent, "429 may pass")
	require.False(t, results[1].Permanent, "503 may pass")
	require.True(t, results[2].Permanent, "a 404 that does not say UNREGISTERED is not a dead token, and a retry cannot fix it")
	require.True(t, results[3].Permanent, "403 SENDER_ID_MISMATCH")
	require.True(t, results[4].Permanent, "400 about the payload")
}

func TestFCMGetsANewAccessTokenAfterBeingRefused(t *testing.T) {
	s := newFCMServer(t)
	p := s.provider(t)

	_, err := p.Send(t.Context(), []push.Message{{Token: "good"}})
	require.NoError(t, err)
	require.EqualValues(t, 1, s.exchange.Load())

	results, err := p.Send(t.Context(), []push.Message{{Token: "unauthorized"}})
	require.NoError(t, err)
	require.Error(t, results[0].Err)
	require.False(t, results[0].Permanent, "a refused OAuth token is retried")

	_, err = p.Send(t.Context(), []push.Message{{Token: "good"}})
	require.NoError(t, err)
	require.EqualValues(t, 2, s.exchange.Load(), "the refused token was dropped, so the next send asked Google again")
}

func TestFCMReusesTheAccessToken(t *testing.T) {
	s := newFCMServer(t)
	p := s.provider(t)

	for range 3 {
		_, err := p.Send(t.Context(), []push.Message{{Token: "good"}})
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, s.exchange.Load())
}

func TestFCMHighPriorityMessage(t *testing.T) {
	s := newFCMServer(t)
	p := s.provider(t)

	_, err := p.Send(t.Context(), []push.Message{
		{Token: "normal", Title: "t", Body: "b", CollapseKey: "n1"},
		{Token: "panic", Title: "t", Body: "b", CollapseKey: "n2", High: true},
	})
	require.NoError(t, err)

	byToken := map[string]map[string]any{}
	for _, m := range s.messages {
		byToken[m["token"].(string)] = m
	}
	android := func(m map[string]any) map[string]any { return m["android"].(map[string]any) }
	androidNotification := func(m map[string]any) map[string]any { return android(m)["notification"].(map[string]any) }
	aps := func(m map[string]any) map[string]any {
		return m["apns"].(map[string]any)["payload"].(map[string]any)["aps"].(map[string]any)
	}

	require.Equal(t, "normal", android(byToken["normal"])["priority"])
	require.NotContains(t, androidNotification(byToken["normal"]), "channel_id")
	require.Equal(t, "n1", android(byToken["normal"])["collapse_key"])
	require.Empty(t, aps(byToken["normal"]))

	require.Equal(t, "high", android(byToken["panic"])["priority"])
	require.Equal(t, "panic", androidNotification(byToken["panic"])["channel_id"])
	require.Equal(t, "siren", androidNotification(byToken["panic"])["sound"])
	require.Equal(t, "critical", aps(byToken["panic"])["interruption-level"])
}

func TestFCMRefusedCredentialsFailTheWholeCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	p, err := push.NewFCMProvider(serviceAccountB64(t, srv.URL), push.WithBaseURL(srv.URL))
	require.NoError(t, err)

	_, err = p.Send(t.Context(), []push.Message{{Token: "good"}})
	require.Error(t, err)
}

func TestNewFCMProviderRejectsBadCredentials(t *testing.T) {
	secret := "c3VwZXItc2VjcmV0"
	for name, in := range map[string]string{
		"not base64":  "%%%not-base64%%%",
		"not json":    base64.StdEncoding.EncodeToString([]byte("nope")),
		"no key":      base64.StdEncoding.EncodeToString([]byte(`{"project_id":"p","client_email":"e"}`)),
		"bad key pem": base64.StdEncoding.EncodeToString([]byte(`{"project_id":"p","client_email":"e","private_key":"` + secret + `"}`)),
	} {
		_, err := push.NewFCMProvider(in)
		require.Error(t, err, name)
		require.NotContains(t, err.Error(), secret, name)
	}
}
