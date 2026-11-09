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
	BaseURL string
	// Token is an optional Hugging Face access token for gated models.
	Token  string
	Client *http.Client
}

// DefaultHub returns a hub for huggingface.co. Downloads have no overall
// timeout because weights take tens of minutes; callers bound them with
// their context.
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
)

// ListFiles returns the repository's files on its main branch.
func (hub Hub) ListFiles(ctx context.Context, repository string) ([]File, error) {
	if !repositoryPattern.MatchString(repository) || strings.Contains(repository, "..") {
		return nil, fmt.Errorf("invalid repository %q", repository)
	}
	request, err := hub.request(ctx, "/api/models/"+repository+"/tree/main?recursive=true")
	if err != nil {
		return nil, err
	}
	response, err := hub.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", repository, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list %s: %s", repository, statusMessage(response))
	}
	var entries []struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Size int64  `json:"size"`
		LFS  *struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"lfs"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&entries); err != nil {
		return nil, fmt.Errorf("list %s: invalid response", repository)
	}
	var files []File
	for _, entry := range entries {
		if entry.Type != "file" || entry.LFS == nil || !digestPattern.MatchString(entry.LFS.OID) || !safeRelative(entry.Path) {
			continue
		}
		files = append(files, File{Path: entry.Path, Size: entry.LFS.Size, SHA256: entry.LFS.OID})
	}
	return files, nil
}

// safeRelative accepts a forward-slash path inside the repository.
func safeRelative(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, "\\") || strings.ContainsAny(path, "\x00\r\n") {
		return false
	}
	return !slices.Contains(strings.Split(path, "/"), "..") && !slices.Contains(strings.Split(path, "/"), ".")
}

// SelectQuant picks the GGUF files of one quantization: a single file or
// every part of a split model, in order. Vision projector files are left
// out.
func SelectQuant(files []File, quant string) ([]File, error) {
	if quant == "" {
		return nil, errors.New("choose a quantization")
	}
	token := regexp.MustCompile(`(?i)(^|[-._/])` + regexp.QuoteMeta(quant) + `([-./]|$)`)
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
	if len(selected) > 1 {
		for _, file := range selected {
			if !splitPart.MatchString(file.Path) {
				return nil, fmt.Errorf("more than one %s GGUF file in the repository", quant)
			}
		}
	}
	return selected, nil
}

var splitPart = regexp.MustCompile(`-\d{5}-of-\d{5}\.gguf$`)

func (hub Hub) request(ctx context.Context, path string) (*http.Request, error) {
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

func (hub Hub) fileURL(repository, path string) string {
	escaped := make([]string, 0, strings.Count(path, "/")+1)
	for _, part := range strings.Split(path, "/") {
		escaped = append(escaped, url.PathEscape(part))
	}
	return "/" + repository + "/resolve/main/" + strings.Join(escaped, "/")
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
