package assets

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed *.js *.css *.png
var files embed.FS

func Handler() http.Handler {
	return http.FileServer(http.FS(files))
}

func Read(name string) ([]byte, error) {
	return fs.ReadFile(files, name)
}
