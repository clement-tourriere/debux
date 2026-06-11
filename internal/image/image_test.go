package image

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/client"
)

func newPullTestClient(t *testing.T, body string) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/images/create") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	t.Cleanup(srv.Close)

	cli, err := client.New(client.WithHost(strings.Replace(srv.URL, "http://", "tcp://", 1)))
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func TestPullImageSurfacesInBandErrors(t *testing.T) {
	// The daemon reports pull failures as in-band JSON on an HTTP 200 stream.
	cli := newPullTestClient(t, `{"status":"Pulling from library/nginx"}
{"errorDetail":{"message":"manifest for nginx:nope not found"},"error":"manifest for nginx:nope not found"}
`)
	err := PullImage(context.Background(), cli, "nginx:nope")
	if err == nil {
		t.Fatal("expected an error from the in-band pull failure, got nil")
	}
	if !strings.Contains(err.Error(), "manifest for nginx:nope not found") {
		t.Fatalf("error should carry the daemon message, got: %v", err)
	}
}

func TestPullImageSucceedsOnCleanStream(t *testing.T) {
	cli := newPullTestClient(t, `{"status":"Pulling from library/nginx"}
{"status":"Status: Downloaded newer image for nginx:latest"}
`)
	if err := PullImage(context.Background(), cli, "nginx:latest"); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

func TestEnsureImageWithPolicyRejectsInvalidPolicy(t *testing.T) {
	cli := newPullTestClient(t, "")
	err := EnsureImageWithPolicy(context.Background(), cli, "nginx", "Sometimes")
	if err == nil || !strings.Contains(err.Error(), "invalid pull policy") {
		t.Fatalf("expected invalid pull policy error, got: %v", err)
	}
}
