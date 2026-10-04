package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"

	"github.com/daltoniam/overload/web/templates/layouts"
)

var scriptPattern = regexp.MustCompile(`(?s)<script>(.*?)</script>`)
var themePolicy = themeScriptPolicy()

func themeScriptPolicy() string {
	var html bytes.Buffer
	if err := layouts.Base(layouts.PageData{}).Render(context.Background(), &html); err != nil {
		panic(err)
	}
	var hashes []string
	for _, match := range scriptPattern.FindAllSubmatch(html.Bytes(), -1) {
		digest := sha256.Sum256(match[1])
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(digest[:])+"'")
	}
	if len(hashes) != 2 {
		panic("theme scripts missing from layout")
	}
	return strings.Join(hashes, " ")
}
