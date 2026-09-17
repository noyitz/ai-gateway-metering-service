package dashboard

import "embed"

//go:embed dashboard.html manager.html admin.html routing.html compression.html login.html user-dashboard.html welcome.html
var FS embed.FS
