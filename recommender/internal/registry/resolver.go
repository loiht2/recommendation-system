package registry

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ImageRef holds parsed components of a container image reference.
type ImageRef struct {
	Registry   string // e.g. "registry.example.com", "ghcr.io", "quay.io"
	Repository string // e.g. "namespace/model"
	Tag        string // e.g. "v1"
	Scheme     string // "https" or "http"
}

// knownRegistries lists hostnames that are definitely registries even when
// appearing as the first path component without a dot+TLD.
var knownRegistries = map[string]bool{
	"localhost": true,
}

// isRegistryHostname heuristically determines whether a string looks like a
// registry hostname rather than a Docker Hub namespace.
//
// Registry indicators:
//   - Contains a dot (ghcr.io, quay.io, registry.example.com, nvcr.io, gcr.io)
//   - Contains a colon (localhost:5000)
//   - Is in the knownRegistries allowlist
func isRegistryHostname(s string) bool {
	if knownRegistries[s] {
		return true
	}
	return strings.Contains(s, ".") || strings.Contains(s, ":")
}

// dockerHubAPIHost is the actual V2 API endpoint for Docker Hub images.
const dockerHubAPIHost = "registry-1.docker.io"

// ParseImageURL parses a Docker image reference into its components.
// It supports images from any OCI / Docker V2 registry, including:
//
//   - Docker Hub:       nginx:latest, library/nginx:1.25, docker.io/library/nginx:1.25
//   - GHCR:             ghcr.io/owner/repo:tag
//   - Quay:             quay.io/org/repo:tag
//   - GCR / Artifact:   gcr.io/project/image:tag
//   - NVCR:             nvcr.io/nvidia/pytorch:tag
//   - ECR:              123456789012.dkr.ecr.us-east-1.amazonaws.com/repo:tag
//   - ACR:              myacr.azurecr.io/repo:tag
//   - Private:          registry.example.com:5000/path/image:tag
//   - Localhost:         localhost:5000/myimage:dev
func ParseImageURL(imageURL string) (*ImageRef, error) {
	imageURL = strings.TrimSpace(imageURL)
	if imageURL == "" {
		return nil, fmt.Errorf("image_url is empty")
	}

	// Determine scheme hint from explicit prefix, then strip it.
	scheme := "https"
	if strings.HasPrefix(imageURL, "http://") {
		scheme = "http"
		imageURL = strings.TrimPrefix(imageURL, "http://")
	} else {
		imageURL = strings.TrimPrefix(imageURL, "https://")
	}

	// Reject digest references — we need a tag to query the registry.
	if strings.Contains(imageURL, "@") {
		return nil, fmt.Errorf("image_url should be a tag reference, not a digest reference; got %q", imageURL)
	}

	// Split off the tag (last colon that is NOT part of a port number).
	tag := "latest"
	if idx := strings.LastIndex(imageURL, ":"); idx > 0 {
		afterColon := imageURL[idx+1:]
		// If there are no slashes after the colon it's a tag; otherwise it's a port.
		if !strings.Contains(afterColon, "/") {
			tag = afterColon
			imageURL = imageURL[:idx]
		}
	}

	// Split the first path component to decide registry vs. Docker Hub namespace.
	var registry, repository string

	parts := strings.SplitN(imageURL, "/", 2)
	switch {
	case len(parts) == 1:
		// Bare name like "nginx" → docker.io/library/nginx
		registry = "docker.io"
		repository = "library/" + parts[0]

	case isRegistryHostname(parts[0]):
		registry = parts[0]
		repository = parts[1]

	default:
		// "namespace/image" without a hostname → Docker Hub
		registry = "docker.io"
		repository = imageURL
	}

	// Normalise Docker Hub to its V2 API hostname.
	if registry == "docker.io" || registry == "index.docker.io" {
		registry = dockerHubAPIHost
	}

	// Docker Hub official images live under "library/".
	if registry == dockerHubAPIHost && !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}

	// Use http for localhost by default (no TLS on dev registries).
	if strings.HasPrefix(registry, "localhost") {
		scheme = "http"
	}

	if repository == "" {
		return nil, fmt.Errorf("could not parse repository from %q", imageURL)
	}

	return &ImageRef{
		Registry:   registry,
		Repository: repository,
		Tag:        tag,
		Scheme:     scheme,
	}, nil
}

// ---------------------------------------------------------------------------
// Resolver
// ---------------------------------------------------------------------------

// Resolver resolves container image tags to their linux/amd64 SHA256 digest
// by querying the Docker Registry HTTP API V2.
//
// It works with any registry that implements the OCI Distribution Spec or
// Docker Registry HTTP API V2, including Docker Hub, GHCR, Quay, GCR, NVCR,
// ECR, ACR, Harbor, and plain HTTP registries.
type Resolver struct {
	client   *http.Client
	user     string
	password string
}

// NewResolver creates a new registry resolver.
// user/password are optional — pass empty strings for anonymous access.
func NewResolver(user, password string) *Resolver {
	return &Resolver{
		client: &http.Client{
			Timeout: 30 * time.Second,
			// Do NOT follow redirects automatically — some registries
			// (ECR, GCR) issue redirects to blob storage and we need
			// to handle auth headers carefully.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// Allow up to 10 redirects but strip the Authorization
				// header on cross-host redirects (security best practice).
				if len(via) >= 10 {
					return fmt.Errorf("too many redirects")
				}
				if req.URL.Host != via[0].URL.Host {
					req.Header.Del("Authorization")
				}
				return nil
			},
		},
		user:     user,
		password: password,
	}
}

// ResolveDigest resolves an image reference to its linux/amd64 SHA256 digest.
// It correctly handles OCI index / Docker manifest list by drilling down
// to the platform-specific manifest.
func (r *Resolver) ResolveDigest(ref *ImageRef) (string, error) {
	auth, err := r.authenticate(ref)
	if err != nil {
		return "", fmt.Errorf("authenticating with %s: %w", ref.Registry, err)
	}

	// Request all supported manifest types — the registry picks the best match.
	acceptTypes := []string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}

	manifestBytes, mediaType, headerDigest, err := r.fetchManifest(ref, ref.Tag, auth, acceptTypes)
	if err != nil {
		return "", fmt.Errorf("fetching manifest for %s/%s:%s: %w", ref.Registry, ref.Repository, ref.Tag, err)
	}

	// If it's a manifest list / OCI index, drill down to linux/amd64.
	if isManifestList(mediaType) {
		return r.resolveFromIndex(ref, auth, manifestBytes)
	}

	// Single-platform manifest — prefer the registry-provided digest.
	if headerDigest != "" {
		return headerDigest, nil
	}
	return computeSHA256(manifestBytes), nil
}

// isManifestList returns true if the media type represents a multi-arch index.
func isManifestList(mediaType string) bool {
	return mediaType == "application/vnd.oci.image.index.v1+json" ||
		mediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
}

// ---------------------------------------------------------------------------
// Authentication — supports Bearer AND Basic schemes
// ---------------------------------------------------------------------------

// authHeader is the value to put in the "Authorization" HTTP header.
// It may be empty (anonymous), "Bearer <token>", or "Basic <b64>".
type authHeader string

// authenticate probes /v2/ and performs the required auth handshake.
// Works with:
//   - Anonymous registries (no auth)
//   - Bearer token registries (Docker Hub, GHCR, Quay, GCR, Harbor)
//   - Basic auth registries (Artifactory, Nexus, some private registries)
func (r *Resolver) authenticate(ref *ImageRef) (authHeader, error) {
	probeURL := fmt.Sprintf("%s://%s/v2/", ref.Scheme, ref.Registry)
	resp, err := r.client.Get(probeURL)
	if err != nil {
		return "", fmt.Errorf("probing registry %s: %w", ref.Registry, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) // drain

	if resp.StatusCode == http.StatusOK {
		return "", nil // anonymous access
	}

	if resp.StatusCode != http.StatusUnauthorized {
		return "", fmt.Errorf("unexpected status %d from %s", resp.StatusCode, probeURL)
	}

	wwwAuth := resp.Header.Get("WWW-Authenticate")
	if wwwAuth == "" {
		// No hint — fall back to Basic auth if we have credentials.
		if r.user != "" && r.password != "" {
			return authHeader("Basic " + base64Encode(r.user, r.password)), nil
		}
		return "", fmt.Errorf("registry %s returned 401 with no WWW-Authenticate header", ref.Registry)
	}

	authScheme := strings.SplitN(wwwAuth, " ", 2)[0]

	switch strings.ToLower(authScheme) {
	case "bearer":
		return r.bearerAuth(wwwAuth, ref)
	case "basic":
		if r.user == "" || r.password == "" {
			return "", fmt.Errorf("registry %s requires Basic auth but no credentials configured", ref.Registry)
		}
		return authHeader("Basic " + base64Encode(r.user, r.password)), nil
	default:
		return "", fmt.Errorf("unsupported auth scheme %q from %s", authScheme, ref.Registry)
	}
}

// bearerAuth performs the OAuth2-style token exchange used by most registries.
func (r *Resolver) bearerAuth(wwwAuth string, ref *ImageRef) (authHeader, error) {
	realm, service, scope := parseWWWAuthenticate(wwwAuth, ref.Repository)
	if realm == "" {
		return "", fmt.Errorf("could not parse realm from WWW-Authenticate: %s", wwwAuth)
	}

	// Build query properly via url.Values to handle special characters.
	tokenURL, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("invalid realm URL %q: %w", realm, err)
	}
	q := tokenURL.Query()
	if service != "" {
		q.Set("service", service)
	}
	q.Set("scope", scope)
	tokenURL.RawQuery = q.Encode()

	req, err := http.NewRequest("GET", tokenURL.String(), nil)
	if err != nil {
		return "", err
	}
	if r.user != "" && r.password != "" {
		req.SetBasicAuth(r.user, r.password)
	}

	tokenResp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting bearer token from %s: %w", tokenURL.Host, err)
	}
	defer tokenResp.Body.Close()

	if tokenResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(tokenResp.Body)
		return "", fmt.Errorf("bearer token request returned %d: %s", tokenResp.StatusCode, string(body))
	}

	var tokenData struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"` // GHCR uses this field
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tokenData); err != nil {
		return "", fmt.Errorf("decoding token response: %w", err)
	}

	tok := tokenData.Token
	if tok == "" {
		tok = tokenData.AccessToken
	}
	if tok == "" {
		return "", fmt.Errorf("empty token from %s", tokenURL.Host)
	}
	return authHeader("Bearer " + tok), nil
}

// base64Encode produces the standard Base64-encoded "user:password" string.
func base64Encode(user, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
}

// parseWWWAuthenticate parses parameters from a Bearer WWW-Authenticate header.
//
// Example headers seen in the wild:
//
//	Docker Hub:  Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"
//	GHCR:        Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:owner/repo:pull"
//	Quay:        Bearer realm="https://quay.io/v2/auth",service="quay.io",scope="repository:org/repo:pull"
//	GCR:         Bearer realm="https://gcr.io/v2/token",service="gcr.io",scope="repository:project/image:pull"
//	Harbor:      Bearer realm="https://harbor.example.com/service/token",service="harbor-registry",scope="repository:project/image:pull"
func parseWWWAuthenticate(header, repository string) (realm, service, scope string) {
	// Strip auth scheme prefix (case-insensitive).
	if idx := strings.Index(header, " "); idx >= 0 {
		header = header[idx+1:]
	}

	params := map[string]string{}
	for _, part := range splitRespectingQuotes(header, ',') {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 {
			params[kv[0]] = strings.Trim(kv[1], "\"")
		}
	}

	realm = params["realm"]
	service = params["service"]
	scope = params["scope"]
	if scope == "" {
		scope = fmt.Sprintf("repository:%s:pull", repository)
	}
	return
}

// splitRespectingQuotes splits a string by sep but ignores sep inside quotes.
func splitRespectingQuotes(s string, sep byte) []string {
	var parts []string
	inQuote := false
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			inQuote = !inQuote
		} else if s[i] == sep && !inQuote {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// ---------------------------------------------------------------------------
// Manifest fetching
// ---------------------------------------------------------------------------

// fetchManifest fetches a manifest by reference (tag or digest).
// Returns body bytes, content type, Docker-Content-Digest header, and error.
func (r *Resolver) fetchManifest(ref *ImageRef, reference string, auth authHeader, acceptTypes []string) ([]byte, string, string, error) {
	u := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", ref.Scheme, ref.Registry, ref.Repository, reference)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, "", "", err
	}

	for _, t := range acceptTypes {
		req.Header.Add("Accept", t)
	}
	if auth != "" {
		req.Header.Set("Authorization", string(auth))
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("fetching manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", "", fmt.Errorf("manifest request for %s returned %d: %s", reference, resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", fmt.Errorf("reading manifest body: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	digest := resp.Header.Get("Docker-Content-Digest")
	return body, contentType, digest, nil
}

// ---------------------------------------------------------------------------
// Multi-arch / OCI index resolution
// ---------------------------------------------------------------------------

// ociIndex represents an OCI Image Index or Docker Manifest List.
type ociIndex struct {
	Manifests []ociManifestDescriptor `json:"manifests"`
}

type ociManifestDescriptor struct {
	MediaType string       `json:"mediaType"`
	Digest    string       `json:"digest"`
	Platform  *ociPlatform `json:"platform,omitempty"`
}

type ociPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

// resolveFromIndex finds the linux/amd64 manifest in an OCI index and returns
// its digest. This is the content-addressable SHA256 in the index, which
// uniquely identifies the platform-specific manifest.
func (r *Resolver) resolveFromIndex(ref *ImageRef, auth authHeader, indexBytes []byte) (string, error) {
	var idx ociIndex
	if err := json.Unmarshal(indexBytes, &idx); err != nil {
		return "", fmt.Errorf("decoding manifest index: %w", err)
	}

	for _, m := range idx.Manifests {
		if m.Platform != nil && m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
			return m.Digest, nil
		}
	}

	var found []string
	for _, m := range idx.Manifests {
		if m.Platform != nil {
			found = append(found, fmt.Sprintf("%s/%s", m.Platform.OS, m.Platform.Architecture))
		}
	}
	return "", fmt.Errorf("no linux/amd64 manifest found in index; available platforms: %v", found)
}

// ---------------------------------------------------------------------------
// Digest computation
// ---------------------------------------------------------------------------

// computeSHA256 returns the "sha256:<hex>" digest of the given bytes.
func computeSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}
