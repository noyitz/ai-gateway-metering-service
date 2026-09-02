package dashboard

import "embed"

//go:embed dashboard.html admin.html routing.html compression.html myaccount.html login.html user-dashboard.html
var FS embed.FS
