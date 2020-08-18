package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func httpTestCookie(c *gin.Context) {
	// Local variables
	w := c.Writer

	// If they have logged in, their cookie should have values that we set in httpLogin.go
	if _, exists := c.Get(jwtMiddleware.IdentityKey); !exists {
		// It would be more correct to send a "StatusUnauthorized" error code,
		// but we do not want to cause an error in the JavaScript console
		// https://httpstatuses.com/
		http.Error(
			w,
			http.StatusText(http.StatusNoContent),
			http.StatusNoContent, // 204
		)
		return
	}

	http.Error(w, http.StatusText(http.StatusOK), http.StatusOK)
}
