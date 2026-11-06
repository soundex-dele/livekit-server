package bootstrap

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"

	"av-platform/business/internal/platform/control"
	foundation "platform-foundation/server/runtime"
)

type AVConfig struct {
	Foundation      foundation.Config
	RedisURL        string
	Memory          bool
	Nodes           []control.Node
	Businesses      []string
	Web             map[string]string
	AdminWeb        string
	TLSCert, TLSKey string
}

func LoadAVConfig() (AVConfig, error) {
	c := AVConfig{Foundation: foundation.DefaultConfig(), RedisURL: os.Getenv("AV_REDIS_URL"), Memory: os.Getenv("AV_EPHEMERAL") == "memory", Businesses: []string{"screen-cast", "conference", "remote-control", "live-studio"}, Web: map[string]string{}}
	// AV enables only the capabilities it consumes. Optional files/SMTP/credits
	// can still be configured through Foundation's complete loader explicitly.
	if _, ok := os.LookupEnv("PF_MODULES"); ok {
		f, e := foundation.FromEnv()
		if e != nil {
			return c, e
		}
		c.Foundation = f
	} else {
		c.Foundation.Modules = []string{"organizations", "jobs", "events"}
		c.Foundation.DSN = os.Getenv("PF_PG_DSN")
		c.Foundation.SQLite = os.Getenv("PF_SQLITE_PATH")
		c.Foundation.PublicOrigin = strings.TrimSuffix(os.Getenv("PF_PUBLIC_ORIGIN"), "/")
		c.Foundation.CookieSecure = strings.HasPrefix(c.Foundation.PublicOrigin, "https://")
		c.Foundation.Registration = os.Getenv("PF_REGISTRATION") == "true"
		if v := os.Getenv("PF_ADDR"); v != "" {
			c.Foundation.Address = v
		}
	}
	if v, ok := os.LookupEnv("AV_MODULES"); ok {
		c.Businesses = nil
		for _, m := range strings.Split(v, ",") {
			if m = strings.TrimSpace(m); m != "" {
				c.Businesses = append(c.Businesses, m)
			}
		}
	}
	for _, m := range []string{"screen-cast", "conference", "remote-control", "live-studio"} {
		c.Web[m] = os.Getenv("AV_" + strings.ToUpper(strings.ReplaceAll(m, "-", "_")) + "_WEB")
	}
	c.AdminWeb = os.Getenv("AV_ADMIN_WEB")
	raw := []byte(os.Getenv("AV_MEDIA_NODES"))
	if name := os.Getenv("AV_MEDIA_NODES_FILE"); name != "" {
		var e error
		raw, e = os.ReadFile(name)
		if e != nil {
			return c, errors.New("cannot read AV_MEDIA_NODES_FILE")
		}
	}
	if len(raw) > 0 {
		if e := json.Unmarshal(raw, &c.Nodes); e != nil {
			return c, errors.New("invalid AV_MEDIA_NODES configuration")
		}
	}
	c.TLSCert, c.TLSKey = os.Getenv("AV_TLS_CERT"), os.Getenv("AV_TLS_KEY")
	return c, c.Validate()
}
func (c AVConfig) Validate() error {
	if e := c.Foundation.Validate(); e != nil {
		return e
	}
	for _, required := range []string{"organizations", "jobs", "events"} {
		if !c.Foundation.Enabled(required) {
			return errors.New("AV requires Foundation organizations,jobs,events modules")
		}
	}
	if c.Memory && c.Foundation.DSN != "" {
		return errors.New("memory ephemeral storage is restricted to SQLite development")
	}
	if !c.Memory && c.RedisURL == "" {
		return errors.New("set AV_REDIS_URL; AV_EPHEMERAL=memory is an explicit local test option")
	}
	seen := map[string]bool{}
	for _, m := range c.Businesses {
		if m != "screen-cast" && m != "conference" && m != "remote-control" && m != "live-studio" {
			return errors.New("unknown AV module")
		}
		if seen[m] {
			return errors.New("duplicate AV module")
		}
		seen[m] = true
	}
	seen = map[string]bool{}
	for _, n := range c.Nodes {
		if n.ID == "" || seen[n.ID] || n.Capacity < 1 || n.RTCKey == "" || n.ManagementKey == "" || n.RTCKey == n.ManagementKey || n.RTCSecret == n.ManagementSecret || len(n.RTCSecret) < 32 || len(n.ManagementSecret) < 32 || len(n.AdmissionSecret) < 32 {
			return errors.New("invalid media node identity, capacity or separate signing credentials")
		}
		seen[n.ID] = true
		u, e := url.Parse(n.APIURL)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
			return errors.New("invalid media API URL")
		}
		u, e = url.Parse(n.PublicURL)
		if e != nil || u.Host == "" || (u.Scheme != "ws" && u.Scheme != "wss") || u.User != nil {
			return errors.New("invalid media signaling URL")
		}
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("set both AV_TLS_CERT and AV_TLS_KEY")
	}
	return nil
}
func (c AVConfig) Enabled(name string) bool {
	for _, v := range c.Businesses {
		if name == v {
			return true
		}
	}
	return false
}
