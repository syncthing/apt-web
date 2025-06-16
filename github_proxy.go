package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	metricGithubRedirects = promauto.NewCounter(prometheus.CounterOpts{
		Name: "github_proxy_redirects_total",
	})
	metricGithubRedirectBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "github_proxy_redirect_bytes_total",
	})
	metricGithubRedirectAssets = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "github_proxy_redirect_assets_loaded",
	})
)

type githubRedirector struct {
	releasesURLs    []string
	refreshInterval time.Duration
	next            http.Handler

	mut    sync.Mutex
	assets map[string]asset
}

type asset struct {
	Name       string `json:"name"`
	BrowserURL string `json:"browser_download_url"`
	Size       int
}

type release struct {
	Tag    string  `json:"tag_name"`
	Assets []asset `json:"assets"`
}

func (r *githubRedirector) Serve(ctx context.Context) error {
	slog.Info("starting GitHub redirector")
	defer slog.Info("stopping GitHub redirector")

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			newAssets := make(map[string]asset)
			nonUnique := make(map[string]struct{})
			for _, url := range r.releasesURLs {
				assets, err := r.fetchGithubReleaseAssets(ctx, url)
				if err != nil {
					return err
				}
				for key, asset := range assets {
					if _, ok := nonUnique[key]; ok {
						continue
					}
					if _, ok := newAssets[key]; ok {
						nonUnique[key] = struct{}{}
						delete(newAssets, key)
						slog.Info("skipping non-unique asset", "key", key)
						continue
					}
					newAssets[key] = asset
				}
			}
			r.mut.Lock()
			r.assets = newAssets
			r.mut.Unlock()
			metricGithubRedirectAssets.Set(float64(len(newAssets)))
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
	asset, ok := r.assets[file]
	// Special case; tildes become dots in GitHub assets...
	if !ok {
		asset, ok = r.assets[strings.Replace(file, "~", ".", 1)]
	}
	r.mut.Unlock()

	if !ok {
		r.next.ServeHTTP(w, req)
		return
	}

	if r.buggyAPTVersion(req) {
		slog.Info("serving proxied for buggy APT", "file", file, "ua", req.Header.Get("User-Agent"))
		r.next.ServeHTTP(w, req)
		return
	}

	slog.Info("serving redirect", "file", file, "ua", req.Header.Get("User-Agent"))
	http.Redirect(w, req, asset.BrowserURL, http.StatusTemporaryRedirect)
	metricGithubRedirects.Inc()
	metricGithubRedirectBytes.Add(float64(asset.Size))
}

func (r *githubRedirector) fetchGithubReleaseAssets(ctx context.Context, url string) (map[string]asset, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var rels []release
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, err
	}

	assets := make(map[string]asset)
	for _, rel := range rels {
		for _, asset := range rel.Assets {
			assets[asset.Name] = asset
		}
	}
	return assets, nil
}

// buggyAPTVersion returns true for APT versions that can't properly handle
// a redirect to a signed object storage URL.
func (r *githubRedirector) buggyAPTVersion(req *http.Request) bool {
	// "Debian APT-CURL/1.0 (1.2.35)"
	// "Debian APT-HTTP/1.3 (1.6.18)"
	// "Debian APT-HTTP/1.3 (2.0.11) non-interactive"
	// "Debian APT-HTTP/1.3 (2.2.4)"
	// "Debian APT-HTTP/1.3 (2.4.13)"
	fields := strings.Fields(req.Header.Get("User-Agent"))
	if len(fields) < 3 {
		return false
	}
	if fields[0] != "Debian" || !strings.HasPrefix(fields[1], "APT-HTTP") {
		return false
	}
	parts := strings.Split(strings.Trim(fields[2], "()"), ".")
	if len(parts) < 2 {
		return false
	}

	// Major versions lower than 2 are guaranteed buggy, higher should be
	// fine. Precisely equals two requires further investigation.
	if maj, err := strconv.ParseInt(parts[0], 10, 32); err != nil {
		return false
	} else if maj < 2 {
		return true
	} else if maj > 2 {
		return false
	}

	// Minor versions lower than 2 are buggy.
	if min, err := strconv.ParseInt(parts[1], 10, 32); err != nil {
		return false
	} else if min < 2 {
		return true
	}

	return false
}
