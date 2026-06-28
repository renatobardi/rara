package litellm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewRequiresBaseURLAndKey(t *testing.T) {
	if _, err := New("", "k"); err == nil {
		t.Error("want error for empty base URL")
	}
	if _, err := New("http://x", ""); err == nil {
		t.Error("want error for empty master key")
	}
	if _, err := New("http://x", "k"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if _, err := New("not-a-url", "k"); err == nil {
		t.Error("want error for malformed base URL (no scheme/host)")
	}
}

func TestAddModelRejectsReservedParam(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "master")
	// A params row trying to override the upstream must be rejected, not sent.
	err := c.AddModel(context.Background(), Model{
		ModelName: "m", Upstream: "groq/good", APIKey: "sk",
		Params: map[string]any{"model": "evil/redirect"},
	})
	if err == nil {
		t.Fatal("want error for reserved param 'model' in Params")
	}
	if called {
		t.Error("gateway must not be called when a reserved param is present")
	}
}

func TestListModelsParsesConfigAndDBModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/info" || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer master" {
			t.Errorf("auth header = %q", got)
		}
		_, _ = io.WriteString(w, `{"data":[
			{"model_name":"groq-llama","litellm_params":{"model":"groq/llama-3.3"},"model_info":{"id":"cfg1","db_model":false}},
			{"model_name":"byo","litellm_params":{"model":"openai/x","api_base":"http://up"},"model_info":{"id":"db1","db_model":true,"rara_fp":"abc"}}
		]}`)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "master")
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("want 2 models, got %d", len(models))
	}
	if models[0].DBModel || models[0].ID != "cfg1" || models[0].Upstream != "groq/llama-3.3" {
		t.Errorf("config model parsed wrong: %+v", models[0])
	}
	if !models[1].DBModel || models[1].Fingerprint != "abc" || models[1].APIBase != "http://up" {
		t.Errorf("db model parsed wrong: %+v", models[1])
	}
}

func TestAddModelSendsKeyAndFingerprintNeverLogsKey(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/new" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"message":"ok"}`)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "master")
	err := c.AddModel(context.Background(), Model{
		ModelName: "groq-llama", Upstream: "groq/llama-3.3", APIKey: "sk-secret",
		InputCost: 0.001, OutputCost: 0.002, Fingerprint: "fp1",
		Params: map[string]any{"temperature": 0.5},
	})
	if err != nil {
		t.Fatalf("AddModel: %v", err)
	}
	lp, _ := gotBody["litellm_params"].(map[string]any)
	if lp["model"] != "groq/llama-3.3" || lp["api_key"] != "sk-secret" || lp["temperature"] != 0.5 {
		t.Errorf("litellm_params wrong: %+v", lp)
	}
	mi, _ := gotBody["model_info"].(map[string]any)
	if mi["rara_fp"] != "fp1" {
		t.Errorf("fingerprint not sent: %+v", mi)
	}
}

func TestAddModelErrorOmitsKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"sk-secret leaked back"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "master")
	err := c.AddModel(context.Background(), Model{ModelName: "m", Upstream: "u", APIKey: "sk-secret"})
	if err == nil {
		t.Fatal("want error on 400")
	}
	if strings.Contains(err.Error(), "sk-secret") {
		t.Errorf("error leaked api_key / response body: %v", err)
	}
}

func TestDeleteModelByID(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/delete" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "master")
	if err := c.DeleteModel(context.Background(), "db1"); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	if gotBody["id"] != "db1" {
		t.Errorf("want id=db1 in body, got %+v", gotBody)
	}
}

func TestHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health/liveliness" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "master")
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
}
