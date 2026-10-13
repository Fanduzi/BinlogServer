package lintguard

// Allowed is a reviewed ignored-error site: how many there are and why each is fine.
type Allowed struct {
	Count  int
	Reason string
}

// allowlist is keyed by Site.Key() ("file:func:kind").
var allowlist = map[string]Allowed{}
