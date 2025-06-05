package main

import (
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"sync"
	"time"

	"calmh.dev/proxy"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/syncthing/syncthing/lib/upgrade"
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
		releasesURL:     "https://api.github.com/repos/syncthing/syncthing/releases?per_page=25",
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
		return http.ListenAndServe(metricsListenAddr, promhttp.Handler())
	}))

	main.Add(asService(func(_ context.Context) error {
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

type githubRedirector struct {
	releasesURL     string
	refreshInterval time.Duration

	mut    sync.Mutex
	assets map[string]string
	next   http.Handler
}

func (r *githubRedirector) Serve(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			assets, err := r.fetchGithubReleaseAssets(ctx, r.releasesURL)
			if err != nil {
				return err
			}
			r.mut.Lock()
			r.assets = assets
			r.mut.Unlock()
			timer.Reset(r.refreshInterval)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (r *githubRedirector) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	file := path.Base(req.URL.Path)
	r.mut.Lock()
	url, ok := r.assets[file]
	r.mut.Unlock()
	if ok {
		http.Redirect(w, req, url, http.StatusTemporaryRedirect)
		return
	}
	r.next.ServeHTTP(w, req)
}

func (r *githubRedirector) fetchGithubReleaseAssets(ctx context.Context, url string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var rels []upgrade.Release
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, err
	}

	assets := make(map[string]string)
	for _, rel := range rels {
		for _, asset := range rel.Assets {
			if path.Ext(asset.Name) == ".deb" {
				assets[asset.Name] = asset.BrowserURL
			}
		}
	}
	return assets, nil
}

type asService func(ctx context.Context) error

func (fn asService) Serve(ctx context.Context) error {
	return fn(ctx)
}
