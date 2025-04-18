package main

import (
	"cmp"
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
)

//go:embed site
var siteFS embed.FS

var (
	listenAddr        = cmp.Or(os.Getenv("LISTEN_ADDRESS"), ":8080")
	metricsListenAddr = cmp.Or(os.Getenv("LISTEN_ADDRESS"), ":8081")
	distsHost         = cmp.Or(os.Getenv("DISTS_HOST"), "https://syncthing-apt.s3.fr-par.scw.cloud")
)

func main() {
	subFS, _ := fs.Sub(fs.FS(siteFS), "site")
	site := http.FS(subFS)
	http.Handle("/", http.FileServer(site))

	proxy, err := newCachingProxy(distsHost, 5*time.Minute)
	if err != nil {
		slog.Error("failed to construct proxy", "error", err)
		os.Exit(2)
	}
	filtered := validateFilename(proxy, []string{
		"InRelease",
		"InRelease.gz",
		"Release",
		"Release.gz",
		"Release.gpg",
		"Release.gpg.gz",
		"Packages",
		"Packages.gz",
		"*.deb",
	})
	http.Handle("/dists/", filtered)

	go func() {
		if err := http.ListenAndServe(metricsListenAddr, promhttp.Handler()); err != nil {
			slog.Error("failed to listen", "server", "metrics", "error", err)
		}
	}()

	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		slog.Error("failed to listen", "server", "main", "error", err)
	}
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
