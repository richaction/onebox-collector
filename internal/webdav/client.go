package webdav

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/richaction/onebox-collector/internal/vault"
)

const noteLimit = 32 << 20

type Client struct {
	Base           *url.URL
	User, Password string
	HTTP           *http.Client
	Vault          *vault.Writer
}

func (c *Client) target(parts ...string) *url.URL {
	return c.Base.JoinPath(parts...)
}

func (c *Client) request(ctx context.Context, method string, target *url.URL, body io.Reader, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, errors.New("invalid WebDAV request")
	}
	for k, values := range headers {
		req.Header[k] = values
	}
	if c.User != "" {
		req.SetBasicAuth(c.User, c.Password)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("WebDAV %s request failed", method)
	}
	return resp, nil
}

func (c *Client) collection(ctx context.Context, target *url.URL) error {
	resp, err := c.request(ctx, "MKCOL", target, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil
	}
	return fmt.Errorf("WebDAV MKCOL HTTP %d", resp.StatusCode)
}

func (c *Client) read(ctx context.Context, target *url.URL) ([]byte, string, bool, error) {
	resp, err := c.request(ctx, http.MethodGet, target, nil, nil)
	if err != nil {
		return nil, "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", false, fmt.Errorf("WebDAV GET HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, noteLimit+1))
	if err != nil || len(data) > noteLimit {
		return nil, "", false, errors.New("WebDAV note unreadable or too large")
	}
	return data, resp.Header.Get("ETag"), true, nil
}

func (c *Client) put(ctx context.Context, target *url.URL, data []byte, contentType string, guard http.Header) (int, error) {
	guard.Set("Content-Type", contentType)
	resp, err := c.request(ctx, http.MethodPut, target, bytes.NewReader(data), guard)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, fmt.Errorf("WebDAV PUT HTTP %d", resp.StatusCode)
}

func (c *Client) syncAsset(ctx context.Context, date, name string) error {
	target := c.target(date, "assets", name)
	headers := http.Header{"If-None-Match": {"*"}}
	resp, err := c.request(ctx, http.MethodHead, target, nil, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	status := resp.StatusCode
	if status != http.StatusOK && status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
		return fmt.Errorf("WebDAV HEAD HTTP %d", status)
	}
	// Vault media are immutable and include the content hash in their names.
	if status == http.StatusOK {
		return nil
	}
	data, err := os.ReadFile(c.Vault.AssetPath(date, name))
	if err != nil {
		return err
	}
	status, err = c.put(ctx, target, data, "application/octet-stream", headers)
	if status == http.StatusPreconditionFailed {
		return nil
	}
	return err
}

type listing struct {
	Responses []struct {
		Href string `xml:"href"`
	} `xml:"response"`
}

func conflictName(date, name string) bool {
	if !strings.HasPrefix(name, date) || !strings.HasSuffix(strings.ToLower(name), ".md") {
		return false
	}
	lower := strings.ToLower(name)
	return strings.Contains(lower, "conflict") || strings.Contains(name, "冲突") || strings.Contains(name, "衝突")
}

func (c *Client) conflicts(ctx context.Context, date string) ([]string, error) {
	target := c.target(date)
	resp, err := c.request(ctx, "PROPFIND", target, nil, http.Header{"Depth": {"1"}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("WebDAV PROPFIND HTTP %d", resp.StatusCode)
	}
	var results listing
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&results); err != nil {
		return nil, errors.New("invalid WebDAV directory listing")
	}
	var names []string
	for _, r := range results.Responses {
		u, err := url.Parse(r.Href)
		if err != nil {
			continue
		}
		name := path.Base(u.Path)
		if conflictName(date, name) {
			names = append(names, name)
		}
	}
	return names, nil
}

func (c *Client) archiveRemote(ctx context.Context, date, name string) error {
	if err := c.collection(ctx, c.target(date, ".conflicts")); err != nil {
		return err
	}
	dest := c.target(date, ".conflicts", time.Now().UTC().Format("20060102T150405.000000000")+"-"+name)
	resp, err := c.request(ctx, "MOVE", c.target(date, name), nil, http.Header{"Destination": {dest.String()}, "Overwrite": {"F"}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return nil
	}
	return fmt.Errorf("WebDAV MOVE HTTP %d", resp.StatusCode)
}

func (c *Client) Sync(ctx context.Context, date string) error {
	if !vault.ValidDate(date) {
		return errors.New("invalid WebDAV date")
	}
	local, assets, err := c.Vault.ReadDay(date)
	if err != nil {
		return err
	}
	if err := c.collection(ctx, c.target(date)); err != nil {
		return err
	}
	if err := c.collection(ctx, c.target(date, "assets")); err != nil {
		return err
	}
	target := c.target(date, date+".md")
	remote, etag, exists, err := c.read(ctx, target)
	if err != nil {
		return err
	}
	merged := local
	if exists {
		merged, err = vault.Merge(local, remote)
		if err != nil {
			return fmt.Errorf("WebDAV main note: %w", err)
		}
	}
	localDir := filepath.Dir(c.Vault.NotePath(date))
	files, err := os.ReadDir(localDir)
	if err != nil {
		return err
	}
	var localCopies []string
	for _, file := range files {
		if !file.Type().IsRegular() || !conflictName(date, file.Name()) {
			continue
		}
		filePath := filepath.Join(localDir, file.Name())
		raw, err := os.ReadFile(filePath)
		if err != nil {
			return err
		}
		merged, err = vault.Merge(merged, raw)
		if err != nil {
			return fmt.Errorf("local conflict %q: %w", file.Name(), err)
		}
		localCopies = append(localCopies, filePath)
	}
	copies, err := c.conflicts(ctx, date)
	if err != nil {
		return err
	}
	remoteCopies := make([]string, 0, len(copies))
	for _, name := range copies {
		raw, _, available, err := c.read(ctx, c.target(date, name))
		if err != nil {
			return err
		}
		if !available {
			continue
		}
		merged, err = vault.Merge(merged, raw)
		if err != nil {
			return fmt.Errorf("WebDAV conflict %q: %w", name, err)
		}
		remoteCopies = append(remoteCopies, name)
	}
	for _, name := range assets {
		if err := c.syncAsset(ctx, date, name); err != nil {
			return err
		}
	}
	if !bytes.Equal(merged, remote) || !exists {
		guard := http.Header{"If-None-Match": {"*"}}
		if exists {
			if etag == "" || strings.HasPrefix(etag, "W/") {
				return errors.New("WebDAV note has no strong ETag")
			}
			guard = http.Header{"If-Match": {etag}}
		}
		if _, err := c.put(ctx, target, merged, "text/markdown; charset=utf-8", guard); err != nil {
			return err
		}
	}
	if err := c.Vault.ReplaceIfCurrent(date, local, merged); err != nil {
		return err
	}
	for _, name := range remoteCopies {
		if err := c.archiveRemote(ctx, date, name); err != nil {
			return err
		}
	}
	if len(localCopies) > 0 {
		archive := filepath.Join(localDir, ".conflicts")
		if err := os.MkdirAll(archive, 0700); err != nil {
			return err
		}
		for _, name := range localCopies {
			dst := filepath.Join(archive, time.Now().UTC().Format("20060102T150405.000000000")+"-"+filepath.Base(name))
			if err := os.Rename(name, dst); err != nil {
				return err
			}
		}
	}
	return nil
}
