package bootstrap

import (
	"io/fs"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestSPAHandlerFallsBackToIndex(t *testing.T) {
	assets := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<h1>demo app</h1>")},
		"app.js":     &fstest.MapFile{Data: []byte("console.log('demo')")},
	}
	handler := spaHandler(fs.FS(assets))
	request := httptest.NewRequest("GET", "/commands/command-1", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != 200 || response.Body.String() != "<h1>demo app</h1>" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
}
