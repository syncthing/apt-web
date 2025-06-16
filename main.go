package main

import (
	"cmp"
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"time"

	"calmh.dev/proxy"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/thejerf/suture/v4"
)

//go:embed site
var siteFS embed.FS

var (
	listenAddr        = cmp.Or(os.Getenv("LISTEN_ADDRESS"), ":8080")
	metricsListenAddr = cmp.Or(os.Getenv("LISTEN_ADDRESS"), ":8081")
	distsHost         = cmp.Or(os.Getenv("DISTS_HOST"), "https://syncthing-apt.s3.fr-par.scw.cloud")
)

func main() {
	main := suture.NewSimple("main")

	// The built in FS serves static files from memory
	subFS, _ := fs.Sub(fs.FS(siteFS), "site")
	site := http.FS(subFS)
	http.Handle("/", http.FileServer(site))

	// The caching proxy serves files from the backend object store
	proxy, err := newCachingProxy(distsHost, 5*time.Minute)
	if err != nil {
		slog.Error("failed to construct proxy", "error", err)
		os.Exit(2)
	}

	// The GitHub redirector serves assets from GitHub releases
	github := &githubRedirector{
		releasesURLs: []string{
			"https://api.github.com/repos/syncthing/syncthing/releases?per_page=15",
			"https://api.github.com/repos/syncthing/discosrv/releases?per_page=5",
			"https://api.github.com/repos/syncthing/relaysrv/releases?per_page=5",
		},
		refreshInterval: 5 * time.Minute,
		next:            proxy,
	}
	main.Add(github)

	// We slightly filter which files we're willing to even try to serve
	filtered := validateFilename(github, []string{
		"*.deb",
		"InRelease",
		"InRelease.gz",
		"Release",
		"Release.gz",
		"Release.gpg",
		"Release.gpg.gz",
		"Packages",
		"Packages.gz",
	})
	http.Handle("/dists/", filtered)

	main.Add(asService(func(_ context.Context) error {
		slog.Info("starting metrics listener", "addr", metricsListenAddr)
		return http.ListenAndServe(metricsListenAddr, promhttp.Handler())
	}))

	main.Add(asService(func(_ context.Context) error {
		slog.Info("starting service listener", "addr", listenAddr)
		return http.ListenAndServe(listenAddr, nil)
	}))

	main.Serve(context.Background())
}

func validateFilename(next http.Handler, names []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		name := path.Base(req.URL.Path)
		for _, valid := range names {
			if ok, _ := path.Match(valid, name); ok {
				next.ServeHTTP(w, req)
				return
			}
		}
		http.NotFound(w, req)
	})
}

func newCachingProxy(next string, cacheTime time.Duration) (http.Handler, error) {
	remote, err := url.Parse(next)
	if err != nil {
		return nil, err
	}
	rev := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(remote)
		},
	}

	return proxy.New(cacheTime, 100, rev), nil
}

type asService func(ctx context.Context) error

func (fn asService) Serve(ctx context.Context) error {
	return fn(ctx)
}
