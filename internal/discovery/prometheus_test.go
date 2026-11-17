package discovery

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
)

func promHandler(t *testing.T, expectedAuth string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if expectedAuth != "" && r.Header.Get("Authorization") != expectedAuth {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"status":"error","errorType":"unauthorized","error":"bad auth"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v1/query"):
			_ = r.ParseForm()
			query := r.FormValue("query")
			if query == "" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"missing query"}`)
				return
			}
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"1"]}]}}`)
		case strings.HasSuffix(r.URL.Path, "/api/v1/series"):
			fmt.Fprint(w, `{"status":"success","data":[{"__name__":"http_server_requests_seconds_count","job":"app","instance":"x"}]}`)
		case strings.HasSuffix(r.URL.Path, "/api/v1/label/__name__/values"):
			fmt.Fprint(w, `{"status":"success","data":["http_server_requests_seconds_count"]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"status":"error","errorType":"not_found","error":"not found"}`)
		}
	})
}

func TestClientPingAndBearerAuth(t *testing.T) {
	server := httptest.NewServer(promHandler(t, "Bearer test-token"))
	defer server.Close()
	client, err := NewClient(Config{URL: server.URL, Timeout: 2 * time.Second, BearerToken: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	value, err := client.Query(context.Background(), "vector(1)")
	if err != nil {
		t.Fatal(err)
	}
	if IsEmpty(value) {
		t.Fatal("expected non-empty query result")
	}
}

func TestClientBasicAuthFailureIsClear(t *testing.T) {
	server := httptest.NewServer(promHandler(t, "Basic dXNlcjpwYXNz"))
	defer server.Close()
	client, err := NewClient(Config{URL: server.URL, Timeout: time.Second, Username: "user", Password: "wrong"})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Ping(context.Background())
	if err == nil {
		t.Fatal("expected auth failure")
	}
	if !strings.Contains(err.Error(), "401") && !strings.Contains(strings.ToLower(err.Error()), "unauthorized") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMetricExistsAndSeries(t *testing.T) {
	server := httptest.NewServer(promHandler(t, ""))
	defer server.Close()
	client, err := NewClient(Config{URL: server.URL, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	exists, err := client.MetricExists(context.Background(), "http_server_requests_seconds_count", time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("expected metric to exist")
	}
	series, err := client.Series(context.Background(), `http_server_requests_seconds_count{job="app"}`, time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || series[0][model.LabelName("job")] != "app" {
		t.Fatalf("unexpected series: %#v", series)
	}
}

func TestMetricExistsFallsBackToSeries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/api/v1/label/__name__/values") {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"status":"error","errorType":"not_found","error":"unsupported"}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/api/v1/series") {
			fmt.Fprint(w, `{"status":"success","data":[{"__name__":"up","job":"app"}]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	client, err := NewClient(Config{URL: server.URL, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	exists, err := client.MetricExists(context.Background(), "up", time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("series fallback did not detect metric")
	}
}

func TestClientTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	defer server.Close()
	client, err := NewClient(Config{URL: server.URL, Timeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(context.Background()); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestEndpointValidation(t *testing.T) {
	cases := []string{
		"ftp://prometheus",
		"http://user:pass@prometheus",
		"http://prometheus/path",
		"http://prometheus?x=1",
	}
	for _, raw := range cases {
		if _, err := NewClient(Config{URL: raw}); err == nil {
			t.Errorf("expected URL %q to be rejected", raw)
		}
	}
}

func TestEmptyDetection(t *testing.T) {
	if !IsEmpty(model.Vector{}) {
		t.Fatal("empty vector not detected")
	}
	if IsEmpty(model.Vector{{Metric: model.Metric{}, Value: 1, Timestamp: 1}}) {
		t.Fatal("non-empty vector reported empty")
	}
}
