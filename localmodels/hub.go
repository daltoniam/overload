// Package localmodels downloads open model weights, keeps track of them on
// disk, checks the local llama.cpp build and runs llama-server as a login
// service, so a Mac can serve catalog models without hand-run commands.
package localmodels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Hub reads model repositories from Hugging Face.
type Hub struct {
	// BaseURL is https://huggingface.co unless a test or mirror sets it.
	// It must be an origin (scheme and host, no path): API and file paths
	// are appended to it.
	BaseURL string
	// Token is an optional Hugging Face access token for gated models. It
	// is sent only to BaseURL's host and its subdomains.
	Token  string
	Client *http.Client
	// Stall aborts a download that receives nothing for this long; zero
	// means two minutes.
	Stall time.Duration
}

// DefaultHub returns a hub for huggingface.co. Downloads have no overall
// timeout because weights take tens of minutes; a stalled transfer is cut
// off instead.
func DefaultHub(token string) Hub {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = time.Minute
	return Hub{BaseURL: "https://huggingface.co", Token: token, Client: &http.Client{Transport: transport}}
}

// File is one file in a model repository. SHA256 is the content digest
// Hugging Face records for files stored in Git LFS (all weights are).
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

var (
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	digestPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	revisionPattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// quantPattern accepts llama.cpp quantization names: Q4_K_M, IQ3_XXS,
	// TQ1_0, BF16, F16, F32, MXFP4.
	quantPattern = regexp.MustCompile(`^(?i)(I?Q[0-9]|TQ[0-9]|BF16|F16|F32|MXFP4)[A-Za-z0-9_]{0,27}$`)
)

// ValidRepository reports whether repository is a Hugging Face owner/name.
func ValidRepository(repository string) bool {
	return repositoryPattern.MatchString(repository) && !strings.Contains(repository, "..")
}

// client returns the HTTP client, refusing redirects to non-https URLs and
// dropping the token on redirects that leave the hub's domain.
func (hub Hub) client() *http.Client {
	base := http.DefaultClient
	if hub.Client != nil {
		base = hub.Client
	}
	client := *base
	hubHost := ""
	if parsed, err := url.Parse(hub.BaseURL); err == nil {
		hubHost = strings.ToLower(parsed.Hostname())
	}
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if request.URL.Scheme != "https" && (request.URL.Scheme != via[0].URL.Scheme || via[0].URL.Scheme == "https") {
			return errors.New("refusing a redirect to a non-https URL")
		}
		host := strings.ToLower(request.URL.Hostname())
		if host != hubHost && !strings.HasSuffix(host, "."+hubHost) {
			request.Header.Del("Authorization")
		}
		return nil
	}
	return &client
}

// Revision returns the commit the repository's main branch points at, so
// one download reads one consistent snapshot.
func (hub Hub) Revision(ctx context.Context, repository string) (string, error) {
	var info struct {
		SHA string `json:"sha"`
	}
	if err := hub.getJSON(ctx, "/api/models/"+repository+"/revision/main", &info, nil); err != nil {
		return "", fmt.Errorf("read %s: %w", repository, err)
	}
	if !revisionPattern.MatchString(info.SHA) {
		return "", fmt.Errorf("read %s: no revision", repository)
	}
	return info.SHA, nil
}

// ListFiles returns the repository's files at revision, following
// pagination.
func (hub Hub) ListFiles(ctx context.Context, repository, revision string) ([]File, error) {
	if !ValidRepository(repository) {
		return nil, fmt.Errorf("invalid repository %q", repository)
	}
	if !revisionPattern.MatchString(revision) {
		return nil, fmt.Errorf("invalid revision %q", revision)
	}
	next := "/api/models/" + repository + "/tree/" + revision + "?recursive=true"
	var files []File
	for page := 0; next != ""; page++ {
		if page == 100 {
			return nil, fmt.Errorf("list %s: too many pages", repository)
		}
		var entries []struct {
			Type string `json:"type"`
			Path string `json:"path"`
			LFS  *struct {
				OID  string `json:"oid"`
				Size int64  `json:"size"`
			} `json:"lfs"`
		}
		var link string
		if err := hub.getJSON(ctx, next, &entries, &link); err != nil {
			return nil, fmt.Errorf("list %s: %w", repository, err)
		}
		for _, entry := range entries {
			if entry.Type != "file" || entry.LFS == nil || !digestPattern.MatchString(entry.LFS.OID) || entry.LFS.Size < 0 || !safeRelative(entry.Path) {
				continue
			}
			files = append(files, File{Path: entry.Path, Size: entry.LFS.Size, SHA256: entry.LFS.OID})
		}
		var err error
		if next, err = hub.nextPage(link); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// nextPage reads a Link header's rel="next" URL, resolved against the hub,
// and returns its path; it must stay on the hub's origin.
func (hub Hub) nextPage(link string) (string, error) {
	for _, part := range strings.Split(link, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok || !strings.Contains(params, `rel="next"`) {
			continue
		}
		target = strings.Trim(strings.TrimSpace(target), "<>")
		base, err := url.Parse(strings.TrimRight(hub.BaseURL, "/") + "/")
		if err != nil {
			return "", err
		}
		parsed, err := url.Parse(target)
		if err != nil {
			return "", errors.New("unexpected pagination link")
		}
		resolved := base.ResolveReference(parsed)
		if resolved.Scheme != base.Scheme || resolved.Host != base.Host || !strings.HasPrefix(resolved.Path, "/api/") {
			return "", errors.New("unexpected pagination link")
		}
		return resolved.RequestURI(), nil
	}
	return "", nil
}

func (hub Hub) getJSON(ctx context.Context, path string, target any, link *string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	request, err := hub.request(ctx, path)
	if err != nil {
		return err
	}
	response, err := hub.client().Do(request)
	if err != nil {
		return requestError(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return errors.New(statusMessage(response))
	}
	if link != nil {
		*link = response.Header.Get("Link")
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(target); err != nil {
		return errors.New("invalid response")
	}
	return nil
}

// requestError hides the URL (which may carry signed query parameters)
// from network errors.
func requestError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// safeRelative accepts a forward-slash path inside the repository.
func safeRelative(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, "\\") || strings.ContainsAny(path, "\x00\r\n") {
		return false
	}
	parts := strings.Split(path, "/")
	return !slices.Contains(parts, "..") && !slices.Contains(parts, ".") && !slices.Contains(parts, "")
}

var splitPart = regexp.MustCompile(`-(\d{5})-of-(\d{5})\.gguf$`)

// SelectQuant picks the GGUF files of one quantization: a single file or
// every part of a split model, in order. Vision projector files are left
// out.
func SelectQuant(files []File, quant string) ([]File, error) {
	if !quantPattern.MatchString(quant) {
		return nil, errors.New("choose a quantization such as Q4_K_M")
	}
	// The quantization starts the name or follows a separator; after `_`
	// only when a letter precedes it (model_Q4_K_M), so XS does not match
	// the end of IQ4_XS.
	token := regexp.MustCompile(`(?i)(^|[-./]|[a-z]_)` + regexp.QuoteMeta(quant) + `([-./]|$)`)
	var selected []File
	for _, file := range files {
		name := strings.ToLower(file.Path)
		if !strings.HasSuffix(name, ".gguf") || strings.Contains(name, "mmproj") || !token.MatchString(file.Path) {
			continue
		}
		selected = append(selected, file)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("no %s GGUF file in the repository", quant)
	}
	slices.SortFunc(selected, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	if len(selected) == 1 && !splitPart.MatchString(selected[0].Path) {
		return selected, nil
	}
	for index, file := range selected {
		match := splitPart.FindStringSubmatch(file.Path)
		if match == nil {
			return nil, fmt.Errorf("more than one %s GGUF file in the repository", quant)
		}
		if match[1] != fmt.Sprintf("%05d", index+1) || match[2] != fmt.Sprintf("%05d", len(selected)) || splitPart.ReplaceAllString(file.Path, "") != splitPart.ReplaceAllString(selected[0].Path, "") {
			return nil, fmt.Errorf("the %s GGUF parts are incomplete or belong to different models", quant)
		}
	}
	return selected, nil
}

func (hub Hub) request(ctx context.Context, path string) (*http.Request, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, errors.New("invalid request path")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(hub.BaseURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "overload")
	if hub.Token != "" {
		request.Header.Set("Authorization", "Bearer "+hub.Token)
	}
	return request, nil
}

func (hub Hub) fileURL(repository, revision, path string) string {
	escaped := make([]string, 0, strings.Count(path, "/")+1)
	for _, part := range strings.Split(path, "/") {
		escaped = append(escaped, url.PathEscape(part))
	}
	return "/" + repository + "/resolve/" + revision + "/" + strings.Join(escaped, "/")
}

func statusMessage(response *http.Response) string {
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "access denied; accept the model's terms on Hugging Face and set HF_TOKEN"
	case http.StatusNotFound:
		return "not found"
	default:
		return response.Status
	}
}
