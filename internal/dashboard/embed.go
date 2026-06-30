package dashboard

import "embed"

//go:embed dashboard.html admin.html routing.html compression.html user-dashboard.html
var FS embed.FS
