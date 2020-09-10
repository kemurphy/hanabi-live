package main

import (
	"net/http"
	"os"
	"path"
	"strconv"
	"text/template"
	"time"

	"github.com/appleboy/gin-jwt/v2"
	"github.com/didip/tollbooth"
	"github.com/didip/tollbooth_gin"
	sentrygin "github.com/getsentry/sentry-go/gin"
	"github.com/gin-contrib/gzip"
	"github.com/gin-gonic/gin"
)

type TemplateData struct {
	// Shared
	WebsiteName string
	Title       string // Used to populate the "<title>" tag
	Domain      string // Used to validate that the user is going to the correct URL
	Version     int
	Compiling   bool // True if we are currently recompiling the TypeScript client
	WebpackPort int

	// Profile
	Name       string
	NamesTitle string

	// History
	History      []*GameHistory
	SpecificSeed bool
	Tags         map[int][]string

	// Scores
	DateJoined                 string
	NumGames                   int
	TimePlayed                 string
	NumGamesSpeedrun           int
	TimePlayedSpeedrun         string
	NumMaxScores               int
	TotalMaxScores             int
	PercentageMaxScores        string
	RequestedNumPlayers        int      // Used on the "Missing Scores" page
	NumMaxScoresPerType        []int    // Used on the "Missing Scores" page
	PercentageMaxScoresPerType []string // Used on the "Missing Scores" page
	SharedMissingScores        bool     // Used on the "Missing Scores" page
	VariantStats               []UserVariantStats

	// Stats
	NumVariants int
	Variants    []VariantStatsData

	// Variants
	BestScores    []int
	MaxScoreRate  string
	MaxScore      int
	AverageScore  string
	NumStrikeouts int
	StrikeoutRate string
	RecentGames   []*GameHistory
}

const (
	// The name supplied to the Gin session middleware can be any arbitrary string
	HTTPSessionName    = "hanabi.sid"
	HTTPSessionTimeout = time.Hour * time.Duration(24*365) // 1 year
	HTTPReadTimeout    = 5 * time.Second
	HTTPWriteTimeout   = 10 * time.Second
)

var (
	domain        string
	useTLS        bool
	GATrackingID  string
	webpackPort   int
	jwtMiddleware *jwt.GinJWTMiddleware

	// HTTPClientWithTimeout is used for sending web requests to external sites,
	// which is used in various middleware
	// We don't want to use the default http.Client because it has no default timeout set
	HTTPClientWithTimeout = &http.Client{
		Timeout: HTTPWriteTimeout,
	}
)

func httpInit() {
	// Read some configuration values from environment variables
	// (they were loaded from the ".env" file in "main.go")
	domain = os.Getenv("DOMAIN")
	if len(domain) == 0 {
		logger.Info("The \"DOMAIN\" environment variable is blank; aborting HTTP initialization.")
		return
	}
	sessionSecret := os.Getenv("SESSION_SECRET")
	if len(sessionSecret) == 0 {
		logger.Info("The \"SESSION_SECRET\" environment variable is blank; " +
			"aborting HTTP initialization.")
		return
	}
	portString := os.Getenv("PORT")
	var port int
	if len(portString) == 0 {
		port = 80
	} else {
		if v, err := strconv.Atoi(portString); err != nil {
			logger.Fatal("Failed to convert the \"PORT\" environment variable to a number.")
			return
		} else {
			port = v
		}
	}
	tlsCertFile := os.Getenv("TLS_CERT_FILE")
	tlsKeyFile := os.Getenv("TLS_KEY_FILE")
	if len(tlsCertFile) != 0 && len(tlsKeyFile) != 0 {
		useTLS = true
		if port == 80 {
			port = 443
		}
	}
	GATrackingID = os.Getenv("GA_TRACKING_ID")
	webpackPortString := os.Getenv("WEBPACK_DEV_SERVER_PORT")
	if len(webpackPortString) == 0 {
		webpackPort = 8080
	} else {
		if v, err := strconv.Atoi(webpackPortString); err != nil {
			logger.Fatal("Failed to convert the \"WEBPACK_DEV_SERVER_PORT\" environment variable to a number.")
			return
		} else {
			webpackPort = v
		}
	}

	// Create a new Gin HTTP router
	gin.SetMode(gin.ReleaseMode)                       // Comment this out to debug HTTP stuff
	httpRouterBase := gin.Default()                        // Has the "Logger" and "Recovery" middleware attached

	// Attach rate-limiting middleware from Tollbooth
	// The limiter works per path request,
	// meaning that a user can only request one specific path every X seconds
	// Thus, this does not impact the ability of a user to download CSS and image files all at once
	// (However, we do not want to use the rate-limiter in development, since we might have multiple
	// tabs open that are automatically-refreshing with webpack-dev-server)
	if !isDev {
		limiter := tollbooth.NewLimiter(2, nil) // Limit each user to 2 requests per second
		limiter.SetMessage(http.StatusText(http.StatusTooManyRequests))
		limiterMiddleware := tollbooth_gin.LimitHandler(limiter)
		httpRouterBase.Use(limiterMiddleware)
	}

	// Set up JWT middleware
	jwtMw, _ := jwt.New(&jwt.GinJWTMiddleware{
		Realm:             "hanabi.live",
		Key:               []byte(sessionSecret),
		Timeout:           HTTPSessionTimeout,
		MaxRefresh:        HTTPSessionTimeout,
		IdentityKey:       "github.com/appleboy/gin-jwt/identity",
		SendCookie:        true,
		SendAuthorization: false,
		CookieName:        HTTPSessionName,
		TokenLookup:       "cookie:" + HTTPSessionName,
		PayloadFunc: func(data interface{}) jwt.MapClaims {
			claims := jwt.MapClaims{}
			if id, ok := data.(int); ok {
				claims["id"] = id
			}
			logger.Info("Got claims=", claims)
			return claims
		},
		IdentityHandler: func(c *gin.Context) interface{} {
			claims := jwt.ExtractClaims(c)
			if id, ok := claims["id"]; ok {
				return int(id.(float64))
			}
			return nil
		},
		Authenticator: func(c *gin.Context) (interface{}, error) {
			httpLogin(c)
			if id, ok := c.Get("HANABI_USER_ID"); ok {
				logger.Info("Got HANABI_USER_ID=", id)
				return id.(int), nil
			}
			return nil, nil
		},
		LoginResponse: func(c *gin.Context, _ int, _ string, _ time.Time) {
			if _, ok := c.Get("HANABI_USER_ID"); ok {
				c.Writer.WriteHeader(http.StatusNoContent)
			}
		},
		LogoutResponse: func(_ *gin.Context, _ int) { /* nop */ },
	})

	if !isDev {
		// Bind the cookie to this specific domain for security purposes
		jwtMw.CookieDomain = domain
		// Only send the cookie over HTTPS:
		// https://www.owasp.org/index.php/Testing_for_cookies_attributes_(OTG-SESS-002)
		jwtMw.SecureCookie = useTLS
		// Mitigate XSS attacks:
		// https://www.owasp.org/index.php/HttpOnly
		jwtMw.CookieHTTPOnly = true
		// Mitigate CSRF attacks:
		// https://developer.mozilla.org/en-US/docs/Web/HTTP/Cookies#SameSite_cookies
		jwtMw.CookieSameSite = http.SameSiteStrictMode
	}

	jwtMiddleware = jwtMw
    gzMiddleware := gzip.Gzip(gzip.DefaultCompression)

	// Initialize Google Analytics
	if len(GATrackingID) > 0 {
		httpRouterBase.Use(httpGoogleAnalytics) // Attach the Google Analytics middleware
	}

	// Attach the Sentry middleware
	if usingSentry {
		httpRouterBase.Use(sentrygin.New(sentrygin.Options{
			// https://github.com/getsentry/sentry-go/blob/master/gin/sentrygin.go
			Repanic: true, // Recommended as per the documentation
			Timeout: HTTPWriteTimeout,
		}))
		httpRouterBase.Use(sentryHTTPAttachMetadata)
	}

	// Create router groups with JWT and GZip middleware
	httpRouterJwt := httpRouterBase.Group("/")
	httpRouterJwt.Use(jwtMw.MiddlewareFunc())
	httpRouter := httpRouterBase.Group("/")
	httpRouter.Use(gzMiddleware) // Add GZip compression middleware

	// Path handlers (for login/logout)
	httpRouter.POST("/login", jwtMw.LoginHandler)
	httpRouter.GET("/logout", jwtMw.LogoutHandler, httpLogout)

	// Path handlers (for websocket and auth test)
	httpRouterJwt.GET("/ws", httpWS)
	httpRouterJwt.GET("/test-cookie", httpTestCookie).Use(gzMiddleware)

	// Path handlers (for the main website)
	httpRouter.GET("/", httpMain)
	httpRouter.GET("/replay", httpMain)
	httpRouter.GET("/replay/:gameID", httpMain)
	httpRouter.GET("/replay/:gameID/:turn", httpMain)
	httpRouter.GET("/shared-replay", httpMain)
	httpRouter.GET("/shared-replay/:gameID", httpMain)
	httpRouter.GET("/shared-replay/:gameID/:turn", httpMain)
	httpRouter.GET("/create-table", httpMain)
	httpRouter.GET("/test", httpMain)
	httpRouter.GET("/test/:testNum", httpMain)

	// Path handlers (for development)
	// ("/dev" is the same as "/" but uses webpack-dev-server to serve JavaScript)
	httpRouter.GET("/dev", httpMain)
	httpRouter.GET("/dev/", httpMain)
	httpRouter.GET("/dev/replay", httpMain)
	httpRouter.GET("/dev/replay/:gameID", httpMain)
	httpRouter.GET("/dev/replay/:gameID/:turn", httpMain)
	httpRouter.GET("/dev/shared-replay", httpMain)
	httpRouter.GET("/dev/shared-replay/:gameID", httpMain)
	httpRouter.GET("/dev/shared-replay/:gameID/:turn", httpMain)
	httpRouter.GET("/dev/create-table", httpMain)
	httpRouter.GET("/dev/test", httpMain)
	httpRouter.GET("/dev/test/:testNum", httpMain)

	// Path handlers for other URLs
	httpRouter.GET("/scores", httpScores)
	httpRouter.GET("/scores/:player1", httpScores)
	httpRouter.GET("/profile", httpScores) // "/profile" is an alias for "/scores"
	httpRouter.GET("/profile/:player1", httpScores)
	httpRouter.GET("/history", httpHistory)
	httpRouter.GET("/history/:player1", httpHistory)
	httpRouter.GET("/history/:player1/:player2", httpHistory)
	httpRouter.GET("/history/:player1/:player2/:player3", httpHistory)
	httpRouter.GET("/history/:player1/:player2/:player3/:player4", httpHistory)
	httpRouter.GET("/history/:player1/:player2/:player3/:player4/:player5", httpHistory)
	httpRouter.GET("/history/:player1/:player2/:player3/:player4/:player5/:player6", httpHistory)
	httpRouter.GET("/missing-scores", httpMissingScores)
	httpRouter.GET("/missing-scores/:player1", httpMissingScores)
	httpRouter.GET("/missing-scores/:player1/:numPlayers", httpMissingScores)
	httpRouter.GET("/shared-missing-scores", httpSharedMissingScores)
	httpRouter.GET("/shared-missing-scores/:player1", httpSharedMissingScores)
	httpRouter.GET("/shared-missing-scores/:player1/:player2", httpSharedMissingScores)
	httpRouter.GET("/shared-missing-scores/:player1/:player2/:player3", httpSharedMissingScores)
	httpRouter.GET("/shared-missing-scores/:player1/:player2/:player3/:player4", httpSharedMissingScores)
	httpRouter.GET("/shared-missing-scores/:player1/:player2/:player3/:player4/:player5", httpSharedMissingScores)
	httpRouter.GET("/shared-missing-scores/:player1/:player2/:player3/:player4/:player5/:player6", httpSharedMissingScores)
	httpRouter.GET("/tags", httpTags)
	httpRouter.GET("/tags/:player1", httpTags)
	httpRouter.GET("/seed", httpSeed)
	httpRouter.GET("/seed/:seed", httpSeed) // Display all games played on a given seed
	httpRouter.GET("/stats", httpStats)
	httpRouter.GET("/variant", httpVariant)
	httpRouter.GET("/variant/:id", httpVariant)
	httpRouter.GET("/tag", httpTag)
	httpRouter.GET("/tag/:tag", httpTag)
	httpRouter.GET("/videos", httpVideos)
	httpRouter.GET("/password-reset", httpPasswordReset)
	httpRouter.POST("/password-reset", httpPasswordResetPost)

	// Path handlers for bots, developers, researchers, etc.
	httpRouter.GET("/export", httpExport)
	httpRouter.GET("/export/:game", httpExport)

	// Other
	httpRouter.Static("/public", path.Join(projectPath, "public"))
	httpRouter.StaticFile("/favicon.ico", path.Join(projectPath, "public", "img", "favicon.ico"))

	if useTLS {
		// Create the LetsEncrypt directory structure
		// (CertBot will look for data in "/.well-known/acme-challenge/####")
		letsEncryptPath := path.Join(projectPath, "letsencrypt")
		wellKnownPath := path.Join(letsEncryptPath, ".well-known")
		acmeChallengePath := path.Join(wellKnownPath, "acme-challenge")
		if _, err := os.Stat(letsEncryptPath); os.IsNotExist(err) {
			if err := os.MkdirAll(acmeChallengePath, 0755); err != nil {
				logger.Fatal("Failed to create the \""+acmeChallengePath+"\" directory:", err)
			}
		}
		if _, err := os.Stat(wellKnownPath); os.IsNotExist(err) {
			if err := os.MkdirAll(acmeChallengePath, 0755); err != nil {
				logger.Fatal("Failed to create the \""+acmeChallengePath+"\" directory:", err)
			}
		}
		if _, err := os.Stat(acmeChallengePath); os.IsNotExist(err) {
			if err := os.MkdirAll(acmeChallengePath, 0755); err != nil {
				logger.Fatal("Failed to create the \""+acmeChallengePath+"\" directory:", err)
			}
		}

		// We want all HTTP requests to be redirected to HTTPS
		// (but make an exception for Let's Encrypt)
		// The Gin router is using the default serve mux,
		// so we need to create a new fresh one for the HTTP handler
		HTTPServeMux := http.NewServeMux()
		HTTPServeMux.Handle(
			"/.well-known/acme-challenge/",
			http.FileServer(http.FileSystem(http.Dir(letsEncryptPath))),
		)
		HTTPServeMux.Handle(
			"/",
			http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				http.Redirect(
					w,
					req,
					"https://"+req.Host+req.URL.String(),
					http.StatusMovedPermanently,
				)
			}),
		)

		// ListenAndServe is blocking, so we need to start listening in a new goroutine
		go func() {
			// We need to create a new http.Server because the default one has no timeouts
			// https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts/
			HTTPRedirectServerWithTimeout := &http.Server{
				Addr:         "0.0.0.0:80", // Listen on all IP addresses
				Handler:      HTTPServeMux,
				ReadTimeout:  HTTPReadTimeout,
				WriteTimeout: HTTPWriteTimeout,
			}
			if err := HTTPRedirectServerWithTimeout.ListenAndServe(); err != nil {
				logger.Fatal("ListenAndServe failed to start on port 80.")
				return
			}
			logger.Fatal("ListenAndServe ended for port 80.")
		}()
	}

	// Start listening and serving requests (which is blocking)
	// We need to create a new http.Server because the default one has no timeouts
	// https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts/
	logger.Info("Listening on port " + strconv.Itoa(port) + ".")
	HTTPServerWithTimeout := &http.Server{
		Addr:         "0.0.0.0:" + strconv.Itoa(port), // Listen on all IP addresses
		Handler:      httpRouterBase,
		ReadTimeout:  HTTPReadTimeout,
		WriteTimeout: HTTPWriteTimeout,
	}
	if useTLS {
		if err := HTTPServerWithTimeout.ListenAndServeTLS(tlsCertFile, tlsKeyFile); err != nil {
			logger.Fatal("ListenAndServeTLS failed:", err)
			return
		}
		logger.Fatal("ListenAndServeTLS ended prematurely.")
	} else {
		if err := HTTPServerWithTimeout.ListenAndServe(); err != nil {
			logger.Fatal("ListenAndServe failed:", err)
			return
		}
		logger.Fatal("ListenAndServe ended prematurely.")
	}
}

// httpServeTemplate combines a standard HTML header with the body for a specific page
// (we want the same HTML header for all pages)
func httpServeTemplate(w http.ResponseWriter, data TemplateData, templateName ...string) {
	viewsPath := path.Join(projectPath, "server", "src", "views")
	layoutPath := path.Join(viewsPath, "layout.tmpl")
	logoPath := path.Join(viewsPath, "logo.tmpl")

	// Turn the slice of file names into a slice of full paths
	for i := 0; i < len(templateName); i++ {
		templateName[i] = path.Join(viewsPath, templateName[i]+".tmpl")
	}

	// Ensure that the layout file exists
	if _, err := os.Stat(layoutPath); os.IsNotExist(err) {
		logger.Error("The layout template does not exist.")
		http.Error(
			w,
			http.StatusText(http.StatusInternalServerError),
			http.StatusInternalServerError,
		)
		return
	}

	// Return a 404 if the template doesn't exist or it is a directory
	if info, err := os.Stat(layoutPath); os.IsNotExist(err) {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	} else if info.IsDir() {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}

	// Append the main layout to our list of layouts
	templateName = append(templateName, layoutPath)

	// Append the nav bar logo to our list of layouts
	templateName = append(templateName, logoPath)

	// Create the template
	var tmpl *template.Template
	if v, err := template.ParseFiles(templateName...); err != nil {
		logger.Error("Failed to create the template:", err.Error())
		http.Error(
			w,
			http.StatusText(http.StatusInternalServerError),
			http.StatusInternalServerError,
		)
		return
	} else {
		tmpl = v
	}

	// Since we are using the GZip middleware, we have to specify the content type,
	// or else the page will be downloaded by the browser as "download.gz"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Add extra data that should be the same for every page request
	data.WebsiteName = WebsiteName

	// Execute the template and send it to the user
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		if isCommonHTTPError(err.Error()) {
			logger.Info("Ordinary error when executing the template: " + err.Error())
		} else {
			logger.Error("Failed to execute the template: " + err.Error())
		}
		http.Error(
			w,
			http.StatusText(http.StatusInternalServerError),
			http.StatusInternalServerError,
		)
	}
}
