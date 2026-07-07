package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fakeOpenAIServer(t *testing.T, status int, resp string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer KEY" {
			t.Errorf("auth header = %q", got)
		}
		if status >= 400 {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(resp))
	}))
}

func TestProvider_Chat_HappyPath(t *testing.T) {
	srv := fakeOpenAIServer(t, http.StatusOK, `{
		"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":2000,"completion_tokens":1000,"total_tokens":3000}
	}`)
	defer srv.Close()

	p := NewDoubao(ProviderCfg{APIKey: "KEY", Model: "doubao-lite-32k", BaseURL: srv.URL})
	resp, err := p.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, Opts{Temperature: 0.7, MaxTokens: 32})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hello" || resp.PromptTokens != 2000 || resp.CompletionTokens != 1000 {
		t.Fatalf("unexpected resp: %+v", resp)
	}
	// doubao-lite-32k: 0.3/1M in, 0.6/1M out -> 2000*0.3/1e6 + 1000*0.6/1e6 = 0.0012
	want := 0.3*2000/1e6 + 0.6*1000/1e6
	if !approx(resp.CostCNY, want, 1e-12) {
		t.Fatalf("cost = %v, want %v", resp.CostCNY, want)
	}
	if resp.CostCNY >= 0.0015 {
		t.Fatalf("lite call cost %v should be well under ¥0.0015 (plan §7.8.3)", resp.CostCNY)
	}
}

func TestProvider_Chat_4xx(t *testing.T) {
	srv := fakeOpenAIServer(t, http.StatusBadRequest, `{"error":{"message":"bad","type":"invalid_request_error","code":"400"}}`)
	defer srv.Close()
	p := NewDeepSeek(ProviderCfg{APIKey: "KEY", Model: "deepseek-chat", BaseURL: srv.URL})
	_, err := p.Chat(context.Background(), []Message{{Role: RoleUser, Content: "x"}}, Opts{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("expected HTTP 400 error, got %v", err)
	}
}

func TestProvider_Chat_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()
	p := NewQwen(ProviderCfg{APIKey: "KEY", Model: "qwen-turbo", BaseURL: srv.URL,
		HTTP: &http.Client{Timeout: 15 * time.Millisecond}})
	_, err := p.Chat(context.Background(), []Message{{Role: RoleUser, Content: "x"}}, Opts{})
	if !errors.Is(err, ErrUpstreamTimeout) {
		t.Fatalf("expected ErrUpstreamTimeout, got %v", err)
	}
}

func TestProvider_Chat_NoAPIKey(t *testing.T) {
	p := NewDoubao(ProviderCfg{Model: "doubao-lite-32k"})
	if _, err := p.Chat(context.Background(), []Message{{Role: RoleUser}}, Opts{}); err == nil {
		t.Fatal("expected error for missing api key")
	}
}

func approx(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}
