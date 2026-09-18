package repository

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

func TestOpenAIOAuthCustomHTTPTokenEndpoint(t *testing.T) {
	requests := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/relay/oauth/token" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		requests <- r.PostForm.Get("grant_type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"test-access","refresh_token":"test-refresh","expires_in":3600}`))
	}))
	defer srv.Close()
	client := NewOpenAIOAuthClient()
	endpoints := openai.OAuthEndpoints{AuthBaseURL: srv.URL + "/relay"}
	if _, err := client.ExchangeCode(context.Background(), "code", "verifier", "", "", "", endpoints); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RefreshTokenWithClientID(context.Background(), "test-refresh", "", "", endpoints); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"authorization_code", "refresh_token"} {
		if got := <-requests; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}
