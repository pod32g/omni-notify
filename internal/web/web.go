// Package web serves the embedded single-page admin UI.
//
// The assets are static (no build step) and shipped inside the binary via
// embed, so the distroless image needs nothing extra. The UI shell is served
// unauthenticated; every data call it makes carries the bearer token the user
// enters in the browser.
package web

import (
	"embed"
	"io/fs"
)

//go:embed assets
var assets embed.FS

// Assets returns the embedded UI asset tree (index.html, app.js, styles.css).
func Assets() fs.FS {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		// The embedded path is a compile-time constant; this cannot fail.
		panic(err)
	}
	return sub
}
