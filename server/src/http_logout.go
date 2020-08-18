package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func httpLogout(c *gin.Context) {
	dev := c.DefaultQuery("dev", "false")

	// We need tell tell the browser to not cache the redirect
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Caching
	// Otherwise, after the first logout, the redirect would be cached, and then on the second
	// logout and beyond, the browser would not actually send a GET request to "/logout"
	c.Writer.Header().Set("Cache-Control", "no-store")

	path := "/"
	if dev == "true" {
		path = "/dev"
	}
	c.Redirect(http.StatusMovedPermanently, path)
}
