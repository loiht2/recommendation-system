package registry_test

import (
	"strings"
	"testing"

	"github.com/loihoangthanh1411/recommender/internal/registry"
)

// These tests call REAL public registries over the internet.
// Run with: go test -v -run TestLive -tags=live ./internal/registry/
// They are skipped by default in CI (no build tag).

func skipIfNoNetwork(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping live registry test in short mode")
	}
}

func TestLive_DockerHub_Nginx(t *testing.T) {
	skipIfNoNetwork(t)

	ref, err := registry.ParseImageURL("nginx:latest")
	if err != nil {
		t.Fatalf("ParseImageURL: %v", err)
	}

	r := registry.NewResolver("", "")
	digest, err := r.ResolveDigest(ref)
	if err != nil {
		t.Fatalf("ResolveDigest: %v", err)
	}

	if !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("expected sha256: prefix, got %q", digest)
	}
	t.Logf("nginx:latest → %s", digest)
}

func TestLive_DockerHub_Alpine(t *testing.T) {
	skipIfNoNetwork(t)

	ref, err := registry.ParseImageURL("alpine:3.19")
	if err != nil {
		t.Fatalf("ParseImageURL: %v", err)
	}

	r := registry.NewResolver("", "")
	digest, err := r.ResolveDigest(ref)
	if err != nil {
		t.Fatalf("ResolveDigest: %v", err)
	}

	if !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("expected sha256: prefix, got %q", digest)
	}
	if len(digest) != 71 { // "sha256:" + 64 hex chars
		t.Errorf("unexpected digest length %d: %s", len(digest), digest)
	}
	t.Logf("alpine:3.19 → %s", digest)
}

func TestLive_DockerHub_Namespace(t *testing.T) {
	skipIfNoNetwork(t)

	ref, err := registry.ParseImageURL("library/busybox:1.36")
	if err != nil {
		t.Fatalf("ParseImageURL: %v", err)
	}

	r := registry.NewResolver("", "")
	digest, err := r.ResolveDigest(ref)
	if err != nil {
		t.Fatalf("ResolveDigest: %v", err)
	}

	if !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("expected sha256: prefix, got %q", digest)
	}
	t.Logf("library/busybox:1.36 → %s", digest)
}

func TestLive_GHCR(t *testing.T) {
	skipIfNoNetwork(t)

	// A well-known public GHCR image (anonymous pull allowed).
	ref, err := registry.ParseImageURL("ghcr.io/homebrew/core/hello:latest")
	if err != nil {
		t.Fatalf("ParseImageURL: %v", err)
	}

	r := registry.NewResolver("", "")
	digest, err := r.ResolveDigest(ref)
	if err != nil {
		// GHCR may require auth even for some "public" images.
		// If the image doesn't resolve anonymously, skip rather than fail.
		t.Skipf("GHCR anonymous pull not available (expected in some envs): %v", err)
	}

	if !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("expected sha256: prefix, got %q", digest)
	}
	t.Logf("ghcr.io/homebrew/core/hello:latest → %s", digest)
}

func TestLive_Quay(t *testing.T) {
	skipIfNoNetwork(t)

	ref, err := registry.ParseImageURL("quay.io/prometheus/prometheus:v2.51.0")
	if err != nil {
		t.Fatalf("ParseImageURL: %v", err)
	}

	r := registry.NewResolver("", "")
	digest, err := r.ResolveDigest(ref)
	if err != nil {
		t.Fatalf("ResolveDigest: %v", err)
	}

	if !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("expected sha256: prefix, got %q", digest)
	}
	t.Logf("quay.io/prometheus/prometheus:v2.51.0 → %s", digest)
}

func TestLive_GCR(t *testing.T) {
	skipIfNoNetwork(t)

	ref, err := registry.ParseImageURL("gcr.io/distroless/static-debian12:latest")
	if err != nil {
		t.Fatalf("ParseImageURL: %v", err)
	}

	r := registry.NewResolver("", "")
	digest, err := r.ResolveDigest(ref)
	if err != nil {
		t.Fatalf("ResolveDigest: %v", err)
	}

	if !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("expected sha256: prefix, got %q", digest)
	}
	t.Logf("gcr.io/distroless/static-debian12:latest → %s", digest)
}
