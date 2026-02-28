package registry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Fake registry server for integration-style unit tests
// ---------------------------------------------------------------------------

// fakeRegistry simulates a Docker V2 registry over httptest.
type fakeRegistry struct {
	server *httptest.Server
	// config
	authScheme string // "bearer", "basic", or "" (anonymous)
	manifests  map[string]fakeManifest
}

type fakeManifest struct {
	contentType string
	body        []byte
	digest      string
}

func newFakeRegistry(authScheme string) *fakeRegistry {
	fr := &fakeRegistry{
		authScheme: authScheme,
		manifests:  make(map[string]fakeManifest),
	}

	// Use a single handler function to route all paths, avoiding
	// ServeMux duplicate-pattern panics.
	fr.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			fr.handleV2(w, r)
			return
		}
		if r.URL.Path == "/token" {
			fr.handleToken(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/manifests/") {
			fr.handleManifests(w, r)
			return
		}
		http.NotFound(w, r)
	}))

	return fr
}

func (fr *fakeRegistry) close() {
	fr.server.Close()
}

func (fr *fakeRegistry) addManifest(repo, reference string, m fakeManifest) {
	key := fmt.Sprintf("%s/%s", repo, reference)
	fr.manifests[key] = m
}

func (fr *fakeRegistry) handleV2(w http.ResponseWriter, r *http.Request) {
	switch fr.authScheme {
	case "bearer":
		host := r.Host
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(
			`Bearer realm="%s/token",service="%s"`, fr.server.URL, host,
		))
		w.WriteHeader(http.StatusUnauthorized)
	case "basic":
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		w.WriteHeader(http.StatusUnauthorized)
	default:
		// Anonymous
		w.WriteHeader(http.StatusOK)
	}
}

func (fr *fakeRegistry) handleToken(w http.ResponseWriter, r *http.Request) {
	// Always grant a token — we're not testing real auth, just the flow.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"token": "fake-bearer-token-12345",
	})
}

func (fr *fakeRegistry) handleManifests(w http.ResponseWriter, r *http.Request) {
	// Path: /v2/{repo...}/manifests/{reference}
	path := strings.TrimPrefix(r.URL.Path, "/v2/")
	idx := strings.LastIndex(path, "/manifests/")
	if idx < 0 {
		http.NotFound(w, r)
		return
	}
	repo := path[:idx]
	reference := path[idx+len("/manifests/"):]
	key := fmt.Sprintf("%s/%s", repo, reference)

	m, ok := fr.manifests[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`))
		return
	}

	// Check auth for bearer scheme
	if fr.authScheme == "bearer" {
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	if fr.authScheme == "basic" {
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Basic ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}

	w.Header().Set("Content-Type", m.contentType)
	if m.digest != "" {
		w.Header().Set("Docker-Content-Digest", m.digest)
	}
	w.WriteHeader(http.StatusOK)
	w.Write(m.body)
}

// ---------------------------------------------------------------------------
// Tests using the fake registry
// ---------------------------------------------------------------------------

func TestResolveDigest_SinglePlatform_Anonymous(t *testing.T) {
	fr := newFakeRegistry("")
	defer fr.close()

	manifestBody := []byte(`{"schemaVersion": 2, "config": {}}`)
	expectedDigest := computeSHA256(manifestBody)

	fr.addManifest("library/myimage", "v1", fakeManifest{
		contentType: "application/vnd.docker.distribution.manifest.v2+json",
		body:        manifestBody,
		digest:      expectedDigest,
	})

	// Build a ref pointing at our fake server.
	host := strings.TrimPrefix(fr.server.URL, "http://")
	ref := &ImageRef{
		Registry:   host,
		Repository: "library/myimage",
		Tag:        "v1",
		Scheme:     "http",
	}

	resolver := NewResolver("", "")
	digest, err := resolver.ResolveDigest(ref)
	if err != nil {
		t.Fatalf("ResolveDigest failed: %v", err)
	}
	if digest != expectedDigest {
		t.Errorf("digest = %q, want %q", digest, expectedDigest)
	}
}

func TestResolveDigest_MultiArch_Bearer(t *testing.T) {
	fr := newFakeRegistry("bearer")
	defer fr.close()

	// The OCI index contains two platforms.
	indexBody, _ := json.Marshal(ociIndex{
		Manifests: []ociManifestDescriptor{
			{
				MediaType: "application/vnd.docker.distribution.manifest.v2+json",
				Digest:    "sha256:amd64digest000000000000000000000000000000000000000000000000000000",
				Platform:  &ociPlatform{OS: "linux", Architecture: "amd64"},
			},
			{
				MediaType: "application/vnd.docker.distribution.manifest.v2+json",
				Digest:    "sha256:arm64digest000000000000000000000000000000000000000000000000000000",
				Platform:  &ociPlatform{OS: "linux", Architecture: "arm64"},
			},
		},
	})

	fr.addManifest("org/multi", "latest", fakeManifest{
		contentType: "application/vnd.docker.distribution.manifest.list.v2+json",
		body:        indexBody,
	})

	host := strings.TrimPrefix(fr.server.URL, "http://")
	ref := &ImageRef{
		Registry:   host,
		Repository: "org/multi",
		Tag:        "latest",
		Scheme:     "http",
	}

	resolver := NewResolver("", "")
	digest, err := resolver.ResolveDigest(ref)
	if err != nil {
		t.Fatalf("ResolveDigest failed: %v", err)
	}

	want := "sha256:amd64digest000000000000000000000000000000000000000000000000000000"
	if digest != want {
		t.Errorf("digest = %q, want %q", digest, want)
	}
}

func TestResolveDigest_OCI_Index(t *testing.T) {
	fr := newFakeRegistry("")
	defer fr.close()

	indexBody, _ := json.Marshal(ociIndex{
		Manifests: []ociManifestDescriptor{
			{
				MediaType: "application/vnd.oci.image.manifest.v1+json",
				Digest:    "sha256:ocilinuxdigest00000000000000000000000000000000000000000000000000",
				Platform:  &ociPlatform{OS: "linux", Architecture: "amd64"},
			},
		},
	})

	fr.addManifest("myns/myimg", "v2", fakeManifest{
		contentType: "application/vnd.oci.image.index.v1+json",
		body:        indexBody,
	})

	host := strings.TrimPrefix(fr.server.URL, "http://")
	ref := &ImageRef{
		Registry:   host,
		Repository: "myns/myimg",
		Tag:        "v2",
		Scheme:     "http",
	}

	resolver := NewResolver("", "")
	digest, err := resolver.ResolveDigest(ref)
	if err != nil {
		t.Fatalf("ResolveDigest failed: %v", err)
	}

	want := "sha256:ocilinuxdigest00000000000000000000000000000000000000000000000000"
	if digest != want {
		t.Errorf("digest = %q, want %q", digest, want)
	}
}

func TestResolveDigest_BasicAuth(t *testing.T) {
	fr := newFakeRegistry("basic")
	defer fr.close()

	manifestBody := []byte(`{"schemaVersion": 2}`)
	expectedDigest := "sha256:abc123"

	fr.addManifest("private/model", "v1", fakeManifest{
		contentType: "application/vnd.docker.distribution.manifest.v2+json",
		body:        manifestBody,
		digest:      expectedDigest,
	})

	host := strings.TrimPrefix(fr.server.URL, "http://")
	ref := &ImageRef{
		Registry:   host,
		Repository: "private/model",
		Tag:        "v1",
		Scheme:     "http",
	}

	resolver := NewResolver("testuser", "testpass")
	digest, err := resolver.ResolveDigest(ref)
	if err != nil {
		t.Fatalf("ResolveDigest failed: %v", err)
	}
	if digest != expectedDigest {
		t.Errorf("digest = %q, want %q", digest, expectedDigest)
	}
}

func TestResolveDigest_NoLinuxAmd64(t *testing.T) {
	fr := newFakeRegistry("")
	defer fr.close()

	indexBody, _ := json.Marshal(ociIndex{
		Manifests: []ociManifestDescriptor{
			{
				MediaType: "application/vnd.docker.distribution.manifest.v2+json",
				Digest:    "sha256:armonly00000000000000000000000000000000000000000000000000000000000",
				Platform:  &ociPlatform{OS: "linux", Architecture: "arm64"},
			},
		},
	})

	fr.addManifest("org/armonly", "latest", fakeManifest{
		contentType: "application/vnd.docker.distribution.manifest.list.v2+json",
		body:        indexBody,
	})

	host := strings.TrimPrefix(fr.server.URL, "http://")
	ref := &ImageRef{
		Registry:   host,
		Repository: "org/armonly",
		Tag:        "latest",
		Scheme:     "http",
	}

	resolver := NewResolver("", "")
	_, err := resolver.ResolveDigest(ref)
	if err == nil {
		t.Fatal("expected error for missing linux/amd64, got nil")
	}
	if !strings.Contains(err.Error(), "no linux/amd64") {
		t.Errorf("error = %q, expected it to mention 'no linux/amd64'", err.Error())
	}
}

func TestResolveDigest_ManifestNotFound(t *testing.T) {
	fr := newFakeRegistry("")
	defer fr.close()
	// No manifests added.

	host := strings.TrimPrefix(fr.server.URL, "http://")
	ref := &ImageRef{
		Registry:   host,
		Repository: "does/not/exist",
		Tag:        "v1",
		Scheme:     "http",
	}

	resolver := NewResolver("", "")
	_, err := resolver.ResolveDigest(ref)
	if err == nil {
		t.Fatal("expected error for missing manifest, got nil")
	}
}

func TestResolveDigest_FallbackComputeDigest(t *testing.T) {
	// When the registry does NOT return Docker-Content-Digest header,
	// we compute it from the manifest body.
	fr := newFakeRegistry("")
	defer fr.close()

	manifestBody := []byte(`{"schemaVersion": 2, "mediaType": "test"}`)
	expectedDigest := computeSHA256(manifestBody)

	fr.addManifest("ns/img", "v1", fakeManifest{
		contentType: "application/vnd.docker.distribution.manifest.v2+json",
		body:        manifestBody,
		digest:      "", // no header digest
	})

	host := strings.TrimPrefix(fr.server.URL, "http://")
	ref := &ImageRef{
		Registry:   host,
		Repository: "ns/img",
		Tag:        "v1",
		Scheme:     "http",
	}

	resolver := NewResolver("", "")
	digest, err := resolver.ResolveDigest(ref)
	if err != nil {
		t.Fatalf("ResolveDigest failed: %v", err)
	}
	if digest != expectedDigest {
		t.Errorf("digest = %q, want %q", digest, expectedDigest)
	}
}
