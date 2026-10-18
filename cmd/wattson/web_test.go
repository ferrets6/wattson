package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRenderIndexTitle(t *testing.T) {
	cases := map[string]string{
		"":         "<title>Wattson</title>",
		"Home Lab": "<title>Wattson · Home Lab</title>",
		"<b>&</b>": "<title>Wattson · &lt;b&gt;&amp;&lt;/b&gt;</title>",
	}
	for siteName, want := range cases {
		page, err := renderIndex(webRoot(), siteName)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(page), want) {
			t.Errorf("SITE_NAME=%q: index.html has no %q", siteName, want)
		}
	}
}

func TestIndexServedWithSiteName(t *testing.T) {
	page, err := renderIndex(webRoot(), "Home Lab")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", indexHandler(page))
	mux.Handle("/", http.FileServer(http.FS(webRoot())))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), "<title>Wattson · Home Lab</title>") {
		t.Errorf("GET / did not serve the rendered title")
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/favicon.svg", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /favicon.svg = %d, want 200", rec.Code)
	}
}
