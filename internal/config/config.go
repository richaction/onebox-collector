package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Corp, Token, AESKey, Secret      string
	Callback, Listen, InternalListen string
	Vault, Data, ReplayToken         string
	MediaLimit                       int64
	DAV                              *url.URL
	DAVUser, DAVPassword             string
	OfficialFetch                    bool
	OfficialTimeout                  time.Duration
	OfficialImages                   int
	OfficialUA                       string
	DouyinDuration                   time.Duration
}

func setting(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func numeric(key string, def, min, max int) (int, error) {
	n, err := strconv.Atoi(setting(key, strconv.Itoa(def)))
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("%s must be between %d and %d", key, min, max)
	}
	return n, nil
}

var callbackPattern = regexp.MustCompile(`^/(?:[A-Za-z0-9_-]+/)*[A-Za-z0-9_-]+$`)

func Load() (Config, error) {
	c := Config{
		Corp: os.Getenv("WECOM_CORP_ID"), Token: os.Getenv("WECOM_TOKEN"), AESKey: os.Getenv("WECOM_AES_KEY"), Secret: os.Getenv("WECOM_SECRET"),
		Callback: setting("CALLBACK_PATH", "/webhook/kf"), Listen: setting("LISTEN", ":8080"), InternalListen: setting("INTERNAL_LISTEN", "127.0.0.1:8081"),
		Vault: setting("VAULT_ROOT", "./collect"), Data: setting("DATA_DIR", "./data"), ReplayToken: os.Getenv("ADMIN_TOKEN"),
		DAVUser: os.Getenv("DAV_USER"), DAVPassword: os.Getenv("DAV_PASS"), OfficialUA: os.Getenv("OFFICIAL_FETCH_UA"),
	}
	var missing []string
	for _, field := range []struct{ key, value string }{
		{"WECOM_CORP_ID", c.Corp}, {"WECOM_TOKEN", c.Token}, {"WECOM_AES_KEY", c.AESKey}, {"WECOM_SECRET", c.Secret},
	} {
		if strings.TrimSpace(field.value) == "" {
			missing = append(missing, field.key)
		}
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("missing configuration: %s", strings.Join(missing, ", "))
	}
	if !callbackPattern.MatchString(c.Callback) {
		return c, fmt.Errorf("CALLBACK_PATH must be a literal non-root path")
	}
	var err error
	c.Vault, err = filepath.Abs(c.Vault)
	if err != nil {
		return c, err
	}
	c.Data, err = filepath.Abs(c.Data)
	if err != nil {
		return c, err
	}
	rel, err := filepath.Rel(c.Vault, c.Data)
	if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return c, fmt.Errorf("DATA_DIR must be outside VAULT_ROOT")
	}
	media, err := numeric("MEDIA_MAX_MB", 20, 1, 512)
	if err != nil {
		return c, err
	}
	c.MediaLimit = int64(media) << 20
	c.OfficialImages, err = numeric("OFFICIAL_FETCH_MAX_IMAGES", 20, 0, 100)
	if err != nil {
		return c, err
	}
	c.OfficialFetch, err = strconv.ParseBool(setting("OFFICIAL_FETCH", "false"))
	if err != nil {
		return c, fmt.Errorf("OFFICIAL_FETCH must be a boolean")
	}
	c.OfficialTimeout, err = time.ParseDuration(setting("OFFICIAL_FETCH_TIMEOUT", "8s"))
	if err != nil || c.OfficialTimeout <= 0 || c.OfficialTimeout > time.Minute {
		return c, fmt.Errorf("OFFICIAL_FETCH_TIMEOUT must be greater than zero and at most 1m")
	}
	c.DouyinDuration, err = time.ParseDuration(setting("DOUYIN_MAX_DURATION", "10m"))
	if err != nil || c.DouyinDuration <= 0 {
		return c, fmt.Errorf("DOUYIN_MAX_DURATION must be positive")
	}
	if dav := os.Getenv("DAV_URL"); dav != "" {
		c.DAV, err = url.Parse(dav)
		if err != nil || c.DAV.Scheme != "https" || c.DAV.Hostname() == "" || c.DAV.User != nil || c.DAV.RawQuery != "" || c.DAV.Fragment != "" {
			return c, fmt.Errorf("DAV_URL must be an HTTPS collection URL without credentials, query or fragment")
		}
		if c.DAVUser == "" || c.DAVPassword == "" {
			return c, fmt.Errorf("DAV_URL requires DAV_USER and DAV_PASS")
		}
	} else if c.DAVUser != "" || c.DAVPassword != "" {
		return c, fmt.Errorf("DAV_USER and DAV_PASS require DAV_URL")
	}
	return c, nil
}
