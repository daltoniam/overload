package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/daltoniam/overload"
	"github.com/daltoniam/overload/postgres"
	"github.com/daltoniam/overload/web/templates/layouts"
	"github.com/daltoniam/overload/web/templates/pages"
	"golang.org/x/net/html"
)

type searchableReader interface {
	SearchRuns(context.Context, postgres.ListFilter) ([]overload.Run, int, error)
	SearchDeliveries(context.Context, postgres.ListFilter) ([]postgres.Delivery, int, error)
}

type listSpec struct{ kinds, statuses []string }

var listSpecs = map[string]listSpec{
	"/settings":               {[]string{"local", "hosted"}, nil},
	"/runs":                   {[]string{"pr_review", "scheduled_prompt"}, []string{"queued", "running", "completed", "failed", "superseded"}},
	"/webhooks":               {nil, []string{"queued", "skipped", "applied"}},
	"/configure/prompts":      {[]string{"entry", "review", "plan", "verify"}, nil},
	"/configure/agents":       {[]string{"pr_review", "scheduled_prompt"}, []string{"Enabled", "Disabled"}},
	"/configure/workflows":    {[]string{"pr_review", "scheduled_prompt"}, []string{"Enabled", "Disabled"}},
	"/configure/repositories": {nil, []string{"Enabled", "Disabled"}},
	"/configure/schedules":    {nil, []string{"Enabled", "Disabled"}},
}

func parseUI(r *http.Request) pages.UIState {
	query := r.URL.Query()
	page, _ := strconv.Atoi(query.Get("page"))
	state := pages.UIState{Query: strings.TrimSpace(query.Get("q")), Kind: query.Get("kind"), Status: query.Get("status"), Page: min(max(page, 1), 100000)}
	if len(state.Query) > 200 {
		state.Query = state.Query[:200]
	}
	if spec, ok := listSpecs[r.URL.Path]; ok {
		if !contains(spec.kinds, state.Kind) {
			state.Kind = ""
		}
		if !contains(spec.statuses, state.Status) {
			state.Status = ""
		}
	}
	return state
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func uiMiddleware(next http.Handler, csrf string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") || r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/webhooks/") || strings.Contains(r.URL.Path, "/artifacts/") || strings.HasPrefix(r.URL.Path, "/setup/") {
			next.ServeHTTP(w, r)
			return
		}
		state := parseUI(r)
		r = r.WithContext(pages.WithUI(r.Context(), state))
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/delete") {
			if !validForm(w, r, csrf) {
				return
			}
			if r.PostForm.Get("confirmed") != "true" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_ = layouts.Base(layouts.PageData{Title: "Confirm deletion"}).Render(templ.WithChildren(r.Context(), pages.Confirmation(r.URL.Path, csrf, r.PostForm)), w)
				return
			}
		}
		recorder := httptest.NewRecorder()
		next.ServeHTTP(recorder, r)
		if total := recorder.Header().Get("X-UI-Total"); total != "" {
			state.Total, _ = strconv.Atoi(total)
			state.Paginated = true
			r = r.WithContext(pages.WithUI(r.Context(), state))
			recorder.Header().Del("X-UI-Total")
		}
		if recorder.Code == http.StatusSeeOther && r.Method == http.MethodPost {
			destination := recorder.Header().Get("Location")
			if parsed, err := url.Parse(destination); err == nil && !parsed.IsAbs() && strings.HasPrefix(parsed.Path, "/") {
				query := parsed.Query()
				query.Set("saved", "true")
				parsed.RawQuery = query.Encode()
				recorder.Header().Set("Location", parsed.String())
			}
		}
		for name, values := range recorder.Header() {
			w.Header()[name] = values
		}
		if r.Method == http.MethodPost && recorder.Code >= 400 && recorder.Code != 403 && recorder.Code != 404 {
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("X-UI-Validation", "true")
				w.WriteHeader(recorder.Code)
				_ = pages.ValidationError(strings.TrimSpace(recorder.Body.String())).Render(r.Context(), w)
				return
			}
			if renderRetainedForm(w, r, next, csrf, recorder) {
				return
			}
		}
		if recorder.Code == http.StatusSeeOther && r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", recorder.Header().Get("Location"))
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodGet && recorder.Code == 200 && strings.Contains(recorder.Body.String(), "<!doctype html>") || r.Method == http.MethodGet && recorder.Code == 200 && strings.Contains(recorder.Body.String(), "<!DOCTYPE html>") {
			body, err := enhanceHTML(r, recorder.Body.Bytes(), state)
			if err == nil {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Add("Vary", "HX-Request")
				w.Header().Add("Vary", "HX-Target")
				_, _ = w.Write(body)
				return
			}
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	})
}

func attr(node *html.Node, name string) string {
	for _, value := range node.Attr {
		if value.Key == name {
			return value.Val
		}
	}
	return ""
}

func setAttr(node *html.Node, name, value string) {
	for index := range node.Attr {
		if node.Attr[index].Key == name {
			node.Attr[index].Val = value
			return
		}
	}
	node.Attr = append(node.Attr, html.Attribute{Key: name, Val: value})
}

func walk(node *html.Node, visit func(*html.Node)) {
	visit(node)
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		walk(child, visit)
	}
}

func text(node *html.Node) string {
	var result strings.Builder
	walk(node, func(child *html.Node) {
		if child.Type == html.TextNode {
			result.WriteString(child.Data)
			result.WriteByte(' ')
		}
	})
	return strings.TrimSpace(result.String())
}

func findNode(node *html.Node, predicate func(*html.Node) bool) *html.Node {
	if predicate(node) {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := findNode(child, predicate); found != nil {
			return found
		}
	}
	return nil
}

func appendComponent(parent *html.Node, component templ.Component, ctx context.Context) error {
	var buffer bytes.Buffer
	if err := component.Render(ctx, &buffer); err != nil {
		return err
	}
	fragment, err := html.ParseFragment(&buffer, &html.Node{Type: html.ElementNode, Data: "div", DataAtom: 0})
	if err != nil {
		doc, parseErr := html.Parse(bytes.NewReader(buffer.Bytes()))
		if parseErr != nil {
			return parseErr
		}
		body := findNode(doc, func(node *html.Node) bool { return node.Data == "body" })
		for body.FirstChild != nil {
			child := body.FirstChild
			body.RemoveChild(child)
			parent.AppendChild(child)
		}
		return nil
	}
	for _, node := range fragment {
		parent.AppendChild(node)
	}
	return nil
}

func enhanceHTML(r *http.Request, body []byte, state pages.UIState) ([]byte, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	main := findNode(doc, func(node *html.Node) bool { return attr(node, "id") == "page-content" })
	if main == nil {
		return body, nil
	}
	if r.URL.Query().Get("saved") == "true" {
		notice := &html.Node{Type: html.ElementNode, Data: "p", Attr: []html.Attribute{{Key: "class", Val: "notice"}, {Key: "role", Val: "status"}}}
		notice.AppendChild(&html.Node{Type: html.TextNode, Data: "Changes saved."})
		main.InsertBefore(notice, main.FirstChild)
	}
	walk(main, func(node *html.Node) {
		if node.Data == "form" && attr(node, "method") == "post" && !strings.HasSuffix(attr(node, "action"), "/delete") {
			setAttr(node, "data-enhance", "true")
		}
	})
	if result := enhanceList(main, r, state); result != nil {
		var buffer bytes.Buffer
		for child := result.FirstChild; child != nil; child = child.NextSibling {
			if err := html.Render(&buffer, child); err != nil {
				return nil, err
			}
		}
		return buffer.Bytes(), nil
	}
	if r.URL.Path == "/settings/new" && r.Header.Get("HX-Target") == "modal-root" && r.Header.Get("HX-Request") == "true" {
		dialog := &html.Node{Type: html.ElementNode, Data: "dialog", Attr: []html.Attribute{{Key: "id", Val: "ui-dialog"}, {Key: "aria-labelledby", Val: "dialog-title"}}}
		header := &html.Node{Type: html.ElementNode, Data: "div", Attr: []html.Attribute{{Key: "class", Val: "dialog-header"}}}
		title := &html.Node{Type: html.ElementNode, Data: "h2", Attr: []html.Attribute{{Key: "id", Val: "dialog-title"}}}
		title.AppendChild(&html.Node{Type: html.TextNode, Data: "New model"})
		header.AppendChild(title)
		closeButton := &html.Node{Type: html.ElementNode, Data: "button", Attr: []html.Attribute{{Key: "type", Val: "button"}, {Key: "class", Val: "secondary"}, {Key: "data-close-dialog", Val: "true"}}}
		closeButton.AppendChild(&html.Node{Type: html.TextNode, Data: "Close"})
		header.AppendChild(closeButton)
		dialog.AppendChild(header)
		content := &html.Node{Type: html.ElementNode, Data: "div", Attr: []html.Attribute{{Key: "class", Val: "dialog-body"}}}
		for main.FirstChild != nil {
			child := main.FirstChild
			main.RemoveChild(child)
			if attr(child, "id") == "form-errors" {
				setAttr(child, "id", "dialog-form-errors")
			}
			if attr(child, "id") != "ui-status" {
				content.AppendChild(child)
			}
		}
		dialog.AppendChild(content)
		var buffer bytes.Buffer
		err := html.Render(&buffer, dialog)
		return buffer.Bytes(), err
	}
	var buffer bytes.Buffer
	err = html.Render(&buffer, doc)
	return buffer.Bytes(), err
}

func enhanceList(main *html.Node, r *http.Request, state pages.UIState) *html.Node {
	spec, ok := listSpecs[r.URL.Path]
	if !ok {
		return nil
	}
	panel := findNode(main, func(node *html.Node) bool {
		return node.Data == "section" && strings.Contains(attr(node, "class"), "panel")
	})
	if panel == nil {
		return nil
	}
	results := &html.Node{Type: html.ElementNode, Data: "div", Attr: []html.Attribute{{Key: "id", Val: "list-results"}, {Key: "class", Val: "results"}}}
	for panel.FirstChild != nil {
		child := panel.FirstChild
		panel.RemoveChild(child)
		results.AppendChild(child)
	}
	if !state.Paginated {
		filterRows(results, r, state)
	}
	_ = appendComponent(panel, pages.SearchToolbar(r.URL.Path, spec.kinds, spec.statuses), r.Context())
	panel.AppendChild(results)
	_ = appendComponent(results, pages.ResultsFooter(r.URL.Path), r.Context())
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") == "list-results" && r.Header.Get("HX-History-Restore-Request") != "true" {
		return results
	}
	return nil
}

func filterRows(results *html.Node, r *http.Request, state pages.UIState) {
	visible := 0
	tbody := findNode(results, func(node *html.Node) bool { return node.Data == "tbody" })
	if tbody != nil {
		for row := tbody.FirstChild; row != nil; {
			nextRow := row.NextSibling
			if row.Data == "tr" {
				if rowMatches(row, r.URL.Path, state) {
					visible++
				} else {
					tbody.RemoveChild(row)
				}
			}
			row = nextRow
		}
		if visible == 0 {
			_ = appendComponent(results, pages.ValidationError("No matching records. Clear the search or change your filters."), r.Context())
		}
	}
	footer := &html.Node{Type: html.ElementNode, Data: "p", Attr: []html.Attribute{{Key: "class", Val: "result-footer"}, {Key: "role", Val: "status"}}}
	footer.AppendChild(&html.Node{Type: html.TextNode, Data: fmt.Sprintf("%d matching records", visible)})
	results.AppendChild(footer)
}

func rowMatches(row *html.Node, path string, state pages.UIState) bool {
	content := strings.ToLower(text(row))
	kind := state.Kind
	if path == "/configure/agents" {
		if kind == "pr_review" {
			kind = "PR review"
		}
		if kind == "scheduled_prompt" {
			kind = "Scheduled prompt"
		}
	}
	return strings.Contains(content, strings.ToLower(state.Query)) && (kind == "" || strings.Contains(content, strings.ToLower(kind))) && (state.Status == "" || strings.Contains(content, strings.ToLower(state.Status)))
}

func renderRetainedForm(w http.ResponseWriter, r *http.Request, next http.Handler, csrf string, failed *httptest.ResponseRecorder) bool {
	if err := r.ParseForm(); err != nil {
		return false
	}
	path := r.URL.Path + "/new"
	if strings.HasSuffix(r.URL.Path, "/delete") {
		path = strings.TrimSuffix(r.URL.Path, "/delete")
	} else if r.URL.Path == "/configure/prompts" && r.PostForm.Get("existing") != "" {
		path = r.URL.Path + "/" + r.PostForm.Get("existing")
	} else if r.URL.Path == "/configure/repositories" && r.PostForm.Get("id") != "" {
		path = r.URL.Path + "/" + r.PostForm.Get("id")
	} else if existing := r.PostForm.Get("existing"); existing != "" {
		path = r.URL.Path + "/" + url.PathEscape(existing)
	}
	if r.URL.Path == "/configure/bindings" {
		return false
	}
	request := r.Clone(r.Context())
	request.Method = http.MethodGet
	request.URL = &url.URL{Path: path}
	request.Body = nil
	request.Form = nil
	request.PostForm = nil
	recorder := httptest.NewRecorder()
	next.ServeHTTP(recorder, request)
	if recorder.Code != 200 {
		return false
	}
	doc, err := html.Parse(bytes.NewReader(recorder.Body.Bytes()))
	if err != nil {
		return false
	}
	walk(doc, func(node *html.Node) {
		name := attr(node, "name")
		if name == "" || name == "csrf" {
			return
		}
		value := r.PostForm.Get(name)
		if node.Data == "input" {
			if attr(node, "type") == "checkbox" {
				removeAttr(node, "checked")
				if value == "true" {
					setAttr(node, "checked", "")
				}
			} else {
				setAttr(node, "value", value)
			}
		}
		if node.Data == "textarea" {
			for node.FirstChild != nil {
				node.RemoveChild(node.FirstChild)
			}
			node.AppendChild(&html.Node{Type: html.TextNode, Data: value})
		}
		if node.Data == "select" {
			walk(node, func(option *html.Node) {
				if option.Data == "option" {
					removeAttr(option, "selected")
					if contains(r.PostForm[name], attr(option, "value")) {
						setAttr(option, "selected", "")
					}
				}
			})
		}
	})
	errors := findNode(doc, func(node *html.Node) bool { return attr(node, "id") == "form-errors" })
	if errors == nil {
		return false
	}
	_ = appendComponent(errors, pages.ValidationError(strings.TrimSpace(failed.Body.String())), r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(failed.Code)
	_ = html.Render(w, doc)
	return true
}

func removeAttr(node *html.Node, name string) {
	for index, value := range node.Attr {
		if value.Key == name {
			node.Attr = append(node.Attr[:index], node.Attr[index+1:]...)
			return
		}
	}
}
