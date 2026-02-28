package registry

import (
	"testing"
)

func TestParseImageURL(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantReg    string
		wantRepo   string
		wantTag    string
		wantScheme string
		wantErr    bool
	}{
		// ---------------------------------------------------------------
		// Docker Hub
		// ---------------------------------------------------------------
		{
			name:       "docker hub bare image",
			input:      "nginx",
			wantReg:    "registry-1.docker.io",
			wantRepo:   "library/nginx",
			wantTag:    "latest",
			wantScheme: "https",
		},
		{
			name:       "docker hub bare image with tag",
			input:      "nginx:1.25",
			wantReg:    "registry-1.docker.io",
			wantRepo:   "library/nginx",
			wantTag:    "1.25",
			wantScheme: "https",
		},
		{
			name:       "docker hub namespace/image",
			input:      "myuser/myapp:v2",
			wantReg:    "registry-1.docker.io",
			wantRepo:   "myuser/myapp",
			wantTag:    "v2",
			wantScheme: "https",
		},
		{
			name:       "docker hub explicit docker.io",
			input:      "docker.io/library/alpine:3.19",
			wantReg:    "registry-1.docker.io",
			wantRepo:   "library/alpine",
			wantTag:    "3.19",
			wantScheme: "https",
		},
		{
			name:       "docker hub index.docker.io",
			input:      "index.docker.io/library/ubuntu:22.04",
			wantReg:    "registry-1.docker.io",
			wantRepo:   "library/ubuntu",
			wantTag:    "22.04",
			wantScheme: "https",
		},
		{
			name:       "docker hub namespace default tag",
			input:      "bitnami/postgresql",
			wantReg:    "registry-1.docker.io",
			wantRepo:   "bitnami/postgresql",
			wantTag:    "latest",
			wantScheme: "https",
		},

		// ---------------------------------------------------------------
		// GHCR (GitHub Container Registry)
		// ---------------------------------------------------------------
		{
			name:       "ghcr.io simple",
			input:      "ghcr.io/owner/repo:v1.0",
			wantReg:    "ghcr.io",
			wantRepo:   "owner/repo",
			wantTag:    "v1.0",
			wantScheme: "https",
		},
		{
			name:       "ghcr.io nested path",
			input:      "ghcr.io/org/sub/image:latest",
			wantReg:    "ghcr.io",
			wantRepo:   "org/sub/image",
			wantTag:    "latest",
			wantScheme: "https",
		},

		// ---------------------------------------------------------------
		// Quay.io
		// ---------------------------------------------------------------
		{
			name:       "quay.io",
			input:      "quay.io/prometheus/prometheus:v2.51.0",
			wantReg:    "quay.io",
			wantRepo:   "prometheus/prometheus",
			wantTag:    "v2.51.0",
			wantScheme: "https",
		},

		// ---------------------------------------------------------------
		// GCR (Google Container Registry)
		// ---------------------------------------------------------------
		{
			name:       "gcr.io",
			input:      "gcr.io/google-containers/pause:3.9",
			wantReg:    "gcr.io",
			wantRepo:   "google-containers/pause",
			wantTag:    "3.9",
			wantScheme: "https",
		},
		{
			name:       "us-docker.pkg.dev (artifact registry)",
			input:      "us-docker.pkg.dev/project/repo/image:sha-abc123",
			wantReg:    "us-docker.pkg.dev",
			wantRepo:   "project/repo/image",
			wantTag:    "sha-abc123",
			wantScheme: "https",
		},

		// ---------------------------------------------------------------
		// NVCR (NVIDIA Container Registry)
		// ---------------------------------------------------------------
		{
			name:       "nvcr.io",
			input:      "nvcr.io/nvidia/pytorch:24.01-py3",
			wantReg:    "nvcr.io",
			wantRepo:   "nvidia/pytorch",
			wantTag:    "24.01-py3",
			wantScheme: "https",
		},

		// ---------------------------------------------------------------
		// AWS ECR
		// ---------------------------------------------------------------
		{
			name:       "ecr",
			input:      "123456789012.dkr.ecr.us-east-1.amazonaws.com/my-repo:v1",
			wantReg:    "123456789012.dkr.ecr.us-east-1.amazonaws.com",
			wantRepo:   "my-repo",
			wantTag:    "v1",
			wantScheme: "https",
		},

		// ---------------------------------------------------------------
		// Azure ACR
		// ---------------------------------------------------------------
		{
			name:       "acr",
			input:      "myacr.azurecr.io/samples/nginx:stable",
			wantReg:    "myacr.azurecr.io",
			wantRepo:   "samples/nginx",
			wantTag:    "stable",
			wantScheme: "https",
		},

		// ---------------------------------------------------------------
		// Private registry with port
		// ---------------------------------------------------------------
		{
			name:       "private registry with port",
			input:      "registry.example.com:5000/team/model:v3",
			wantReg:    "registry.example.com:5000",
			wantRepo:   "team/model",
			wantTag:    "v3",
			wantScheme: "https",
		},

		// ---------------------------------------------------------------
		// Localhost (development)
		// ---------------------------------------------------------------
		{
			name:       "localhost with port",
			input:      "localhost:5000/myimage:dev",
			wantReg:    "localhost:5000",
			wantRepo:   "myimage",
			wantTag:    "dev",
			wantScheme: "http",
		},
		{
			name:       "localhost bare",
			input:      "localhost/myimage:test",
			wantReg:    "localhost",
			wantRepo:   "myimage",
			wantTag:    "test",
			wantScheme: "http",
		},

		// ---------------------------------------------------------------
		// Scheme handling
		// ---------------------------------------------------------------
		{
			name:       "explicit https scheme stripped",
			input:      "https://ghcr.io/owner/img:v1",
			wantReg:    "ghcr.io",
			wantRepo:   "owner/img",
			wantTag:    "v1",
			wantScheme: "https",
		},
		{
			name:       "explicit http scheme preserved",
			input:      "http://myregistry.local/repo:latest",
			wantReg:    "myregistry.local",
			wantRepo:   "repo",
			wantTag:    "latest",
			wantScheme: "http",
		},

		// ---------------------------------------------------------------
		// Error cases
		// ---------------------------------------------------------------
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "digest reference rejected",
			input:   "nginx@sha256:abcdef1234567890",
			wantErr: true,
		},
		{
			name:    "whitespace only",
			input:   "   ",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := ParseImageURL(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for input %q, got nil", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for input %q: %v", tt.input, err)
			}

			if ref.Registry != tt.wantReg {
				t.Errorf("Registry = %q, want %q", ref.Registry, tt.wantReg)
			}
			if ref.Repository != tt.wantRepo {
				t.Errorf("Repository = %q, want %q", ref.Repository, tt.wantRepo)
			}
			if ref.Tag != tt.wantTag {
				t.Errorf("Tag = %q, want %q", ref.Tag, tt.wantTag)
			}
			if ref.Scheme != tt.wantScheme {
				t.Errorf("Scheme = %q, want %q", ref.Scheme, tt.wantScheme)
			}
		})
	}
}

func TestParseWWWAuthenticate(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		repo       string
		wantRealm  string
		wantSvc    string
		wantScope  string
	}{
		{
			name:      "docker hub",
			header:    `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`,
			repo:      "library/nginx",
			wantRealm: "https://auth.docker.io/token",
			wantSvc:   "registry.docker.io",
			wantScope: "repository:library/nginx:pull",
		},
		{
			name:      "ghcr",
			header:    `Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:owner/repo:pull"`,
			repo:      "owner/repo",
			wantRealm: "https://ghcr.io/token",
			wantSvc:   "ghcr.io",
			wantScope: "repository:owner/repo:pull",
		},
		{
			name:      "quay",
			header:    `Bearer realm="https://quay.io/v2/auth",service="quay.io",scope="repository:org/repo:pull"`,
			repo:      "org/repo",
			wantRealm: "https://quay.io/v2/auth",
			wantSvc:   "quay.io",
			wantScope: "repository:org/repo:pull",
		},
		{
			name:      "no scope in header",
			header:    `Bearer realm="https://gcr.io/v2/token",service="gcr.io"`,
			repo:      "project/image",
			wantRealm: "https://gcr.io/v2/token",
			wantSvc:   "gcr.io",
			wantScope: "repository:project/image:pull",
		},
		{
			name:      "harbor",
			header:    `Bearer realm="https://harbor.example.com/service/token",service="harbor-registry"`,
			repo:      "project/myimage",
			wantRealm: "https://harbor.example.com/service/token",
			wantSvc:   "harbor-registry",
			wantScope: "repository:project/myimage:pull",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			realm, svc, scope := parseWWWAuthenticate(tt.header, tt.repo)
			if realm != tt.wantRealm {
				t.Errorf("realm = %q, want %q", realm, tt.wantRealm)
			}
			if svc != tt.wantSvc {
				t.Errorf("service = %q, want %q", svc, tt.wantSvc)
			}
			if scope != tt.wantScope {
				t.Errorf("scope = %q, want %q", scope, tt.wantScope)
			}
		})
	}
}

func TestComputeSHA256(t *testing.T) {
	// Known test vector: sha256 of empty string
	got := computeSHA256([]byte(""))
	want := "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Errorf("computeSHA256(\"\") = %q, want %q", got, want)
	}
}

func TestIsManifestList(t *testing.T) {
	if !isManifestList("application/vnd.oci.image.index.v1+json") {
		t.Error("expected OCI index to be manifest list")
	}
	if !isManifestList("application/vnd.docker.distribution.manifest.list.v2+json") {
		t.Error("expected Docker manifest list to be manifest list")
	}
	if isManifestList("application/vnd.docker.distribution.manifest.v2+json") {
		t.Error("expected single manifest NOT to be manifest list")
	}
	if isManifestList("application/vnd.oci.image.manifest.v1+json") {
		t.Error("expected OCI manifest NOT to be manifest list")
	}
}
