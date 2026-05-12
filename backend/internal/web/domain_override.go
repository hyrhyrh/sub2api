package web

import (
	"encoding/json"
	"os"
	"strings"
)

type DomainOverride struct {
	Name     string `json:"name"`
	Subtitle string `json:"subtitle"`
}

var domainOverrides map[string]DomainOverride

func init() {
	domainOverrides = parseDomainOverrides(os.Getenv("SITE_DOMAIN_OVERRIDES"))
}

func parseDomainOverrides(envValue string) map[string]DomainOverride {
	envValue = strings.TrimSpace(envValue)
	if envValue == "" {
		return nil
	}
	var result map[string]DomainOverride
	if err := json.Unmarshal([]byte(envValue), &result); err != nil {
		return nil
	}
	return result
}

func GetDomainOverride(host string) (DomainOverride, bool) {
	if domainOverrides == nil {
		return DomainOverride{}, false
	}
	host = strings.Split(host, ":")[0]
	override, ok := domainOverrides[host]
	return override, ok
}
