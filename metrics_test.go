package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/api/option"
)

// stubFCM replaces the Firebase seam for the duration of a test.
func stubFCM(t *testing.T, id string, err error) {
	t.Helper()
	original := sendFCM
	sendFCM = func(context.Context, *messaging.Message) (string, error) { return id, err }
	t.Cleanup(func() { sendFCM = original })
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeFCM points the Firebase seam at a real messaging client whose transport
// answers every request with the given FCM v1 response. The messaging.Is* helpers
// only recognise errors the SDK itself parsed, so outcomes can't be stubbed directly.
func fakeFCM(t *testing.T, status int, body string) {
	t.Helper()
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}
	ctx := context.Background()
	app, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: "test"}, option.WithHTTPClient(httpClient))
	if err != nil {
		t.Fatalf("firebase app: %v", err)
	}
	client, err := app.Messaging(ctx)
	if err != nil {
		t.Fatalf("messaging client: %v", err)
	}
	original := sendFCM
	sendFCM = client.Send
	t.Cleanup(func() { sendFCM = original })
}

func fcmError(status, errorCode string) string {
	return `{"error":{"status":"` + status + `","message":"test","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"` + errorCode + `"}]}}`
}

func post(t *testing.T, body string, contentLength int64) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/_gomuks/push/fcm", strings.NewReader(body))
	r.RemoteAddr = "192.0.2.1:4242"
	if contentLength != 0 {
		r.ContentLength = contentLength
	}
	w := httptest.NewRecorder()
	instrumentHandler("push", handlePushProxy)(w, r)
	return w
}

// scrape renders the registry exactly as the /metrics endpoint would.
func scrape(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(metricsHandler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read scrape: %v", err)
	}
	return string(body)
}

func TestRejectionsAreRecorded(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		contentLength int64
		wantStatus    int
		wantReason    string
	}{
		{"invalid json", "not json", 0, http.StatusBadRequest, rejectInvalidJSON},
		{"empty owner", `{"owner":"","token":"t"}`, 0, http.StatusBadRequest, rejectInvalidOwner},
		{"no token or fid", `{"owner":"@a:b"}`, 0, http.StatusBadRequest, rejectInvalidTarget},
		{"both token and fid", `{"owner":"@a:b","token":"t","fid":"f"}`, 0, http.StatusBadRequest, rejectInvalidTarget},
		{"app id not whitelisted", `{"owner":"@a:b","token":"t","app_id":"com.example.evil"}`, 0, http.StatusForbidden, rejectAppIDNotAllowed},
		{"content too large", `{"owner":"@a:b"}`, maxRequestLength + 1, http.StatusRequestEntityTooLarge, rejectContentTooLarge},
		{"payload too large", `{"owner":"@a:b","payload":"` + strings.Repeat("A", 4004) + `"}`, 0, http.StatusRequestEntityTooLarge, rejectPayloadTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			counter := pushesRejectedTotal.WithLabelValues(tc.wantReason)
			before := testutil.ToFloat64(counter)

			if w := post(t, tc.body, tc.contentLength); w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if after := testutil.ToFloat64(counter); after != before+1 {
				t.Errorf("pushes_rejected_total{reason=%q} = %v, want %v", tc.wantReason, after, before+1)
			}
		})
	}
}

func TestPushOutcomesAreRecorded(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantResult string
	}{
		{"success", http.StatusOK, `{"name":"projects/test/messages/msg-id"}`, http.StatusOK, resultSent},
		{"unregistered token", http.StatusNotFound, fcmError("NOT_FOUND", "UNREGISTERED"), http.StatusNotFound, resultInvalidToken},
		{"sender mismatch", http.StatusForbidden, fcmError("PERMISSION_DENIED", "SENDER_ID_MISMATCH"), http.StatusNotFound, resultInvalidToken},
		{"quota exceeded", http.StatusTooManyRequests, fcmError("RESOURCE_EXHAUSTED", "QUOTA_EXCEEDED"), http.StatusTooManyRequests, resultQuotaExceeded},
		{"invalid argument", http.StatusBadRequest, fcmError("INVALID_ARGUMENT", "INVALID_ARGUMENT"), http.StatusBadRequest, resultInvalidArgument},
		{"upstream failure", http.StatusInternalServerError, fcmError("INTERNAL", "INTERNAL"), http.StatusInternalServerError, resultError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeFCM(t, tc.status, tc.body)
			counter := pushesTotal.WithLabelValues(tc.wantResult, "high")
			before := testutil.ToFloat64(counter)

			w := post(t, `{"owner":"@user:example.com","token":"tok","payload":"aGk=","high_priority":true}`, 0)
			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if after := testutil.ToFloat64(counter); after != before+1 {
				t.Errorf("pushes_total{result=%q,priority=high} = %v, want %v", tc.wantResult, after, before+1)
			}
		})
	}
}

func TestScrapeExposesExpectedSeries(t *testing.T) {
	stubFCM(t, "msg-id", nil)
	post(t, `{"owner":"@user:example.com","token":"tok","payload":"aGk="}`, 0)
	out := scrape(t)

	for _, want := range []string{
		`gomuks_push_pushes_total{priority="normal",result="sent"}`,
		`gomuks_push_pushes_rejected_total{reason="invalid_json"}`,
		`gomuks_push_http_requests_total{code="200",handler="push",method="post"}`,
		`gomuks_push_http_request_duration_seconds_bucket`,
		`gomuks_push_fcm_send_duration_seconds_bucket`,
		`gomuks_push_push_payload_bytes_bucket`,
		`gomuks_push_build_info`,
		`go_goroutines`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scrape is missing %q", want)
		}
	}
}

// TestScrapeLeaksNoSecrets guards the cardinality/PII decision: push tokens are
// bearer credentials and source IPs are unbounded, so neither may ever reach the
// metrics endpoint — regardless of how the owner label is configured.
func TestScrapeLeaksNoSecrets(t *testing.T) {
	stubFCM(t, "msg-id", nil)
	post(t, `{"owner":"@user:example.com","token":"super-secret-token","payload":"aGk="}`, 0)
	out := scrape(t)

	for _, forbidden := range []string{"super-secret-token", "192.0.2.1"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("scrape leaked %q", forbidden)
		}
	}
	// The owner is opt-in, so it must stay absent unless explicitly enabled.
	if !ownerLabelEnabled && strings.Contains(out, "@user:example.com") {
		t.Error("scrape exposed the owner without METRICS_OWNER_LABEL")
	}
}

// TestOwnerLabelOptIn covers the METRICS_OWNER_LABEL=true path, which the default
// test run never reaches.
func TestOwnerLabelOptIn(t *testing.T) {
	if !ownerLabelEnabled {
		t.Skip("set METRICS_OWNER_LABEL=true to exercise the owner label")
	}
	stubFCM(t, "msg-id", nil)
	post(t, `{"owner":"@user:example.com","token":"super-secret-token","payload":"aGk="}`, 0)
	out := scrape(t)

	if !strings.Contains(out, `gomuks_push_pushes_by_owner_total{owner="@user:example.com",result="sent"}`) {
		t.Error("owner series missing while METRICS_OWNER_LABEL is enabled")
	}
	if strings.Contains(out, "super-secret-token") {
		t.Error("scrape leaked the push token")
	}
}
