package httpcheck

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestGetReturnsStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	status, body, err := Get(t.Context(), srv.URL, "", time.Second, false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if status != http.StatusCreated {
		t.Errorf("status = %d, want %d", status, http.StatusCreated)
	}
	if body != "hello" {
		t.Errorf("body = %q, want %q", body, "hello")
	}
}

func TestGetSendsHostHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "cms.example.test" {
			http.Error(w, "wrong host", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	status, _, err := Get(t.Context(), srv.URL, "cms.example.test", time.Second, false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if status != http.StatusNoContent {
		t.Errorf("status = %d, want %d", status, http.StatusNoContent)
	}
}

func TestGetRejectsInvalidURL(t *testing.T) {
	if _, _, err := Get(t.Context(), "://not-a-url", "", time.Second, false); err == nil {
		t.Fatal("expected an error for an invalid URL")
	}
}

func TestGetHonoursTheTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	if _, _, err := Get(t.Context(), srv.URL, "", 10*time.Millisecond, false); err == nil {
		t.Fatal("expected a timeout error")
	}
}

func TestLBReachable(t *testing.T) {
	const vhost = "cms.example.test"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != vhost {
			http.Error(w, "wrong host", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	t.Run("https", func(t *testing.T) {
		srv := httptest.NewTLSServer(handler)
		defer srv.Close()
		if !LBReachable(t.Context(), hostOf(t, srv.URL), vhost) {
			t.Fatal("LBReachable = false, want true")
		}
	})

	t.Run("http fallback", func(t *testing.T) {
		srv := httptest.NewServer(handler)
		defer srv.Close()
		if !LBReachable(t.Context(), hostOf(t, srv.URL), vhost) {
			t.Fatal("LBReachable = false, want true")
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		if LBReachable(t.Context(), "127.0.0.1:1", vhost) {
			t.Fatal("LBReachable = true, want false")
		}
	})
}

func TestLBReachableHTTPSRequiresTLS(t *testing.T) {
	const vhost = "cms.example.test"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != vhost {
			http.Error(w, "wrong host", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	t.Run("tls", func(t *testing.T) {
		srv := httptest.NewTLSServer(handler)
		defer srv.Close()
		if !LBReachableHTTPS(t.Context(), hostOf(t, srv.URL), vhost) {
			t.Fatal("LBReachableHTTPS = false, want true")
		}
	})

	t.Run("plain http is not accepted", func(t *testing.T) {
		srv := httptest.NewServer(handler)
		defer srv.Close()
		host := hostOf(t, srv.URL)
		// The permissive probe still accepts the HTTP answer, but the strict
		// one must not: the VIP scenario claims TLS termination.
		if !LBReachable(t.Context(), host, vhost) {
			t.Fatal("LBReachable = false, want true")
		}
		if LBReachableHTTPS(t.Context(), host, vhost) {
			t.Fatal("LBReachableHTTPS = true for a plain-HTTP server, want false")
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		if LBReachableHTTPS(t.Context(), "127.0.0.1:1", vhost) {
			t.Fatal("LBReachableHTTPS = true, want false")
		}
	})
}

func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return u.Host
}
