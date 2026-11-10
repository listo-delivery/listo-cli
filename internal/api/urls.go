package api

import (
	"net/url"
	"strings"
)

// The production addresses, and the environment variables that move them to a
// workspace or to staging.
const (
	DefaultAPIURL   = "https://api.listo.delivery"
	DefaultCableURL = "wss://cable.listo.delivery/cable"

	EnvAPIURL   = "LISTO_API_URL"
	EnvLoginURL = "LISTO_LOGIN_URL"
	EnvCableURL = "LISTO_CABLE_URL"
)

// URLs are the three hosts the CLI talks to.
type URLs struct {
	API   string // the Partner Delivery API, e.g. https://api.listo.delivery
	Login string // the sign-in host (device grant, /oauth/*); "" when it cannot be derived
	Cable string // the live socket, e.g. wss://cable.listo.delivery/cable
}

// ResolveURLs reads LISTO_API_URL, LISTO_LOGIN_URL and LISTO_CABLE_URL.
//
// The sign-in host is the API host with its first label "api" replaced by
// "login" (https://login.listo.delivery for production, and
// https://login.local.listo.delivery:5012 for a workspace whose API is
// https://api.local.listo.delivery:5012), unless LISTO_LOGIN_URL says
// otherwise. An API address whose host does not start with "api." has no
// derivable sign-in host: Login is then "" and the login command must tell the
// person to set LISTO_LOGIN_URL. The socket is NOT derived from the API
// address: set LISTO_CABLE_URL (a workspace serves it at
// wss://api.local.listo.delivery:<port>/cable).
//
// Trailing slashes are trimmed everywhere.
func ResolveURLs(getenv func(string) string) URLs {
	u := URLs{
		API:   trim(getenv(EnvAPIURL)),
		Login: trim(getenv(EnvLoginURL)),
		Cable: trim(getenv(EnvCableURL)),
	}
	if u.API == "" {
		u.API = DefaultAPIURL
	}
	if u.Login == "" {
		u.Login = DeriveLoginURL(u.API)
	}
	if u.Cable == "" {
		u.Cable = DefaultCableURL
	}
	return u
}

// DeriveLoginURL swaps the first host label "api" for "login", keeping scheme,
// port and path. It returns "" when the host does not start with "api.".
func DeriveLoginURL(apiURL string) string {
	p, err := url.Parse(apiURL)
	if err != nil || p.Host == "" {
		return ""
	}
	host := p.Hostname()
	if !strings.HasPrefix(host, "api.") {
		return ""
	}
	newHost := "login." + strings.TrimPrefix(host, "api.")
	if port := p.Port(); port != "" {
		newHost += ":" + port
	}
	p.Host = newHost
	return strings.TrimRight(p.String(), "/")
}

func trim(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }
