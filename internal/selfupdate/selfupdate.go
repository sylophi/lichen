// Package selfupdate fetches lichen release binaries from GitHub and
// swaps them over the running executable: `lichen update` drives it by
// hand, and the version gate drives it automatically when the sync repo
// outversions this build.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"

	"lichen/internal/version"
)

const releaseRepo = "dittofleet/lichen"

// httpGet fetches url, treating any non-200 as an error. The timeout
// covers the whole exchange, body read included. Callers close the body.
func httpGet(url, accept string, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return resp, nil
}

// LatestTag returns the tag_name of the latest GitHub release.
func LatestTag() (string, error) {
	resp, err := httpGet("https://api.github.com/repos/"+releaseRepo+"/releases/latest", "application/vnd.github+json", 10*time.Second)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var data struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	if data.TagName == "" {
		return "", fmt.Errorf("release metadata has no tag_name")
	}
	return data.TagName, nil
}

// download streams the tagged release asset for this platform into path
// (mode 0755), returning the byte count. The caller cleans up on error.
// Asset names here, install.sh's download URL, and the release
// workflow's build matrix must all agree.
func download(tag, path string) (int64, error) {
	var arch string
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		arch = "arm64"
	case "darwin/amd64":
		arch = "x64"
	default:
		return 0, fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/lichen-darwin-%s", releaseRepo, tag, arch)
	resp, err := httpGet(url, "", 5*time.Minute)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return n, copyErr
	}
	return n, closeErr
}

// Install downloads tag's binary for this platform and renames it over
// the running executable.
func Install(tag string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	tmp := self + ".update"
	n, err := download(tag, tmp)
	if err != nil {
		os.Remove(tmp)
		return err
	}
	// Reject a download too small to be a real build (an error page or a
	// truncated transfer).
	if n < 1_000_000 {
		os.Remove(tmp)
		return fmt.Errorf("downloaded file is suspiciously small (%d bytes), aborting", n)
	}
	if err := os.Rename(tmp, self); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Required installs the latest release for a build the sync repo has
// outversioned, first verifying the release actually satisfies repoV: a
// deleted release could otherwise have every machine downloading a build
// that is still too old, forever.
func Required(repoV string) (string, error) {
	tag, err := LatestTag()
	if err != nil {
		return "", fmt.Errorf("fetching release info: %w", err)
	}
	if !version.Valid(tag) || version.Compare(tag, repoV) < 0 {
		return "", fmt.Errorf("the sync repo requires %s but the latest release is %s: publish a newer release, or lower %s in the sync repo", repoV, tag, version.Marker)
	}
	if err := Install(tag); err != nil {
		return "", err
	}
	return tag, nil
}
