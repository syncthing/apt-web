package main

import (
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"calmh.dev/proxy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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

	metricFileRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "aptweb_file_requests_total",
	}, []string{"source"})
	metricRedirectAssets = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "aptweb_redirect_assets_loaded",
	})
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

type githubRedirector struct {
	releasesURLs    []string
	refreshInterval time.Duration
	next            http.Handler

	mut    sync.Mutex
	assets map[string]string
}

func (r *githubRedirector) Serve(ctx context.Context) error {
	slog.Info("starting GitHub redirector")
	defer slog.Info("stopping GitHub redirector")

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			newAssets := make(map[string]string)
			for _, url := range r.releasesURLs {
				assets, err := r.fetchGithubReleaseAssets(ctx, url)
				if err != nil {
					return err
				}
				maps.Copy(newAssets, assets)
			}
			r.mut.Lock()
			r.assets = newAssets
			r.mut.Unlock()
			metricRedirectAssets.Set(float64(len(newAssets)))
			timer.Reset(r.refreshInterval)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (r *githubRedirector) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	file := path.Base(req.URL.Path)
	if unesc, err := url.PathUnescape(file); err == nil {
		file = unesc
	}

	r.mut.Lock()
	url, ok := r.assets[file]
	// Special case; tildes become dots in GitHub assets...
	if !ok {
		url, ok = r.assets[strings.Replace(file, "~", ".", 1)]
	}
	r.mut.Unlock()

	if ok {
		slog.Info("serving redirect", "file", file, "ua", req.Header.Get("User-Agent"))
		http.Redirect(w, req, url, http.StatusTemporaryRedirect)
		metricFileRequests.WithLabelValues("redirect").Inc()
		return
	}

	r.next.ServeHTTP(w, req)
	metricFileRequests.WithLabelValues("proxy").Inc()
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
