package pages

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

type UIState struct {
	Query     string
	Kind      string
	Status    string
	Page      int
	Total     int
	Paginated bool
}

type uiKey struct{}

func WithUI(ctx context.Context, state UIState) context.Context {
	return context.WithValue(ctx, uiKey{}, state)
}

func uiState(ctx context.Context) UIState {
	state, _ := ctx.Value(uiKey{}).(UIState)
	return state
}

func matches(ctx context.Context, kind, status string, values ...string) bool {
	state := uiState(ctx)
	return (state.Kind == "" || state.Kind == kind) && (state.Status == "" || strings.EqualFold(state.Status, status)) && strings.Contains(strings.ToLower(strings.Join(values, " ")), strings.ToLower(state.Query))
}

func pageURL(ctx context.Context, path string, page int) string {
	state := uiState(ctx)
	query := url.Values{}
	query.Set("q", state.Query)
	query.Set("kind", state.Kind)
	query.Set("status", state.Status)
	query.Set("page", strconv.Itoa(page))
	return path + "?" + query.Encode()
}

func safeEndpoint(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "Invalid endpoint"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}
