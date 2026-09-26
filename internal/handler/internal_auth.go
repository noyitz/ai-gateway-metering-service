package handler

import (
	"crypto/subtle"
	"net/http"
)

const internalTokenHeader = "X-PriceTag-Internal-Token"

// RequireInternalAPI protects machine-to-machine endpoints that are called by
// the gateway but must never be exposed as unauthenticated public APIs.
// The comparison is constant-time and the token is never included in a
// response or log message.
func RequireInternalAPI(expected string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get(internalTokenHeader)
		if expected == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			http.Error(w, "internal authentication required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
